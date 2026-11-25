import { useEffect, useRef, useState } from 'react';
import { useInfiniteQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useToast } from '../app/toast';
import { isTransientNetworkError } from '../api/errors';
import { TaskApprovalCard, type TaskApprovalDecision } from '../components/TaskApprovalCard';
import type { components } from '../api/schema';
import './Reviews.css';

type TaskApprovalItem = components['schemas']['TaskApprovalItem'];
const pageSize = 5;

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
  const [page, setPage] = useState(1);
  const approvals = useInfiniteQuery({
    queryKey: ['task-approvals', 'all'],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam }) => portalApi.taskApprovals({ cursor: pageParam }),
    getNextPageParam: (result, pages) => result.next_cursor &&
      !pages.slice(0, -1).some((item) => item.next_cursor === result.next_cursor) ? result.next_cursor : undefined,
    refetchInterval: (query) => query.state.data?.pages.some((result) =>
      result.items.some((item) => item.approval.status === 'pending')) ? 5000 : 30000,
  });
  const { fetchNextPage, isFetchNextPageError, isFetchingNextPage, hasNextPage, refetch } = approvals;
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
      notify(`${request.decision === 'approve' ? '已允许本次操作' : '已拒绝本次操作'}，任务后续状态等待更新。`, 'success');
    },
    onError: (error, request) => {
      void refreshApprovalState(request.item);
      notify(`审核未确认：${errorDetails(error).message}`, 'error');
    },
  });
  const pages = approvals.data?.pages ?? [];
  const items = [...new Map(pages.flatMap((result) => result.items)
    .map((item) => [`${item.task_id}:${item.approval.id}`, item])).values()];
  const pending = items.filter((item) => item.approval.status === 'pending');
  const pendingCount = pages[0]?.pending_count;
  const pageCount = Math.max(1, pendingCount === undefined
    ? Math.ceil(pending.length / pageSize) + (hasNextPage ? 1 : 0)
    : Math.ceil(pendingCount / pageSize));
  const currentPage = Math.min(page, pageCount);
  if (page !== currentPage) setPage(currentPage);
  const firstOnPage = (currentPage - 1) * pageSize;
  const targetCount = Math.min(currentPage * pageSize, pendingCount ?? currentPage * pageSize);
  const currentItems = pending.slice(firstOnPage, firstOnPage + pageSize);
  const lastCursor = pages.at(-1)?.next_cursor;
  const cursorLoop = Boolean(lastCursor && pages.slice(0, -1).some((result) => result.next_cursor === lastCursor));
  const showApprovalData = approvals.isSuccess ||
    ((approvalsNetworkError || approvals.isFetchNextPageError) && Boolean(approvals.data));
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

  useEffect(() => {
    if (showApprovalData && pending.length < targetCount && hasNextPage && !isFetchingNextPage &&
      !isFetchNextPageError && !cursorLoop) void fetchNextPage();
  }, [showApprovalData, pending.length, targetCount, hasNextPage, isFetchingNextPage,
    isFetchNextPageError, cursorLoop, fetchNextPage]);

  const approvalKey = (item: TaskApprovalItem, decision: TaskApprovalDecision) => {
    const scope = `approval:${item.task_id}:${item.approval.id}`;
    const fingerprint = JSON.stringify({ version: item.approval.version, decision });
    const previous = actionKeys.current.get(scope);
    if (previous?.fingerprint === fingerprint) return previous.key;
    const key = newIdempotencyKey();
    actionKeys.current.set(scope, { fingerprint, key });
    return key;
  };
  const renderItem = (item: TaskApprovalItem) => <TaskApprovalCard key={`${item.task_id}:${item.approval.id}`}
    compact reviewInbox approval={item.approval}
    submitting={respond.isPending && respond.variables?.item.approval.id === item.approval.id}
    onDecision={(decision) => respond.mutateAsync({ item, decision, key: approvalKey(item, decision) })} />;
  const hasPending = pendingCount === undefined ? pending.length > 0 || Boolean(hasNextPage) : pendingCount > 0;
  const canNext = currentPage < pageCount && !isFetchingNextPage && !cursorLoop &&
    (pending.length >= targetCount || Boolean(hasNextPage));
  const rangeStart = pendingCount === 0 ? 0 : firstOnPage + 1;
  const rangeEnd = pendingCount === undefined
    ? Math.min(currentPage * pageSize, pending.length)
    : Math.min(currentPage * pageSize, pendingCount);
  const pageNumbers = [...new Set([1, currentPage - 1, currentPage, currentPage + 1, pageCount])]
    .filter((number) => number >= 1 && number <= pageCount).sort((a, b) => a - b);

  return <div className="reviews-page">
    <header className="reviews-heading">
      <div><h1>审核</h1><p>只显示需要你允许或拒绝的操作；任务追问、等待和异常状态请在任务动态查看。</p></div>
    </header>
    {approvals.isPending && <p className="reviews-loading" role="status">正在读取审核事项…</p>}
    {listError && <ReviewsError title={listErrorTitle} message={listErrorMessage} onRetry={retryApprovals} />}
    {showApprovalData && <>
      {pendingCount === undefined && <ReviewsError title="待处理数量尚未确认"
        message="已加载的审批仍可查看，但总数尚未确认。刷新后再核对。" onRetry={() => void refetch()} />}
      {currentItems.length > 0 ? <section className="reviews-list" aria-label="待处理审核操作">
        {currentItems.map(renderItem)}
      </section> : pendingCount === 0 ? <p className="reviews-empty">当前没有需要你确认的操作。</p>
        : isFetchingNextPage ? <p className="reviews-empty" role="status">正在读取后续审核事项…</p>
          : pendingCount === undefined && pending.length === 0 && hasNextPage ?
            <p className="reviews-empty" role="status">正在核对待处理审核事项…</p>
            : <p className="reviews-empty" role="status">待处理数量与列表内容尚未核对完成，请刷新确认。</p>}
      {approvals.isFetchNextPageError && <ReviewsError title="更多审核事项未能加载"
        message={`${errorDetails(approvals.error).message}。已加载内容仍保留。`} onRetry={() => void fetchNextPage()} />}
      {cursorLoop && <ReviewsError title="审核列表需要刷新"
        message="分页游标重复，尚未加载的事项状态未确认。" onRetry={() => void refetch()} />}
      {pendingCount !== undefined && pending.length < pendingCount && !hasNextPage && !isFetchingNextPage && !cursorLoop &&
        <ReviewsError title="审核列表尚未完整"
          message="服务端待处理数量与已加载的审批不一致，请刷新核对后再操作。" onRetry={() => void refetch()} />}
      {hasPending && <nav className="reviews-pagination" aria-label="审核分页">
        <span className="reviews-pagination-range">
          {rangeStart}–{rangeEnd}{pendingCount === undefined ? ` 项 · 已加载 ${pending.length} 项` : ` / ${pendingCount} 项`}
        </span>
        <div className="reviews-pagination-controls">
          <button type="button" className="button button-secondary" disabled={currentPage <= 1 || isFetchingNextPage}
            onClick={() => setPage(currentPage - 1)}>上一页</button>
          {pageNumbers.map((number, index) => <span className="reviews-pagination-number" key={number}>
            {index > 0 && number - pageNumbers[index - 1]! > 1 && <span aria-hidden="true">…</span>}
            <button type="button" className={`button ${number === currentPage ? 'button-primary' : 'button-secondary'}`}
              aria-current={number === currentPage ? 'page' : undefined}
              aria-label={`第 ${number} 页`} disabled={isFetchingNextPage && number !== currentPage}
              onClick={() => setPage(number)}>{number}</button>
          </span>)}
          <button type="button" className="button button-secondary" disabled={!canNext}
            onClick={() => setPage(currentPage + 1)}>{isFetchingNextPage ? '加载中…' : '下一页'}</button>
        </div>
      </nav>}
    </>}
  </div>;
}
