import { Link } from 'react-router';
import { useMutation, useInfiniteQuery, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { errorDetails, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { SchedulePanel } from './SchedulePanel';

export type TaskStatus = Awaited<ReturnType<typeof portalApi.tasks>>['items'][number]['status'];

const activeStatuses = new Set<TaskStatus>(['queued', 'running', 'waiting_user', 'pausing', 'cancelling']);
export const isActiveTaskStatus = (status: TaskStatus) => activeStatuses.has(status);
export const taskStatusLabel: Record<TaskStatus, string> = {
  queued: '排队中', running: '执行中', waiting_user: '等待你处理', pausing: '正在暂停', paused: '已暂停',
  cancelling: '正在取消', completed: '已完成', failed: '失败', cancelled: '已取消',
};

export function Tasks() {
  const queryClient = useQueryClient();
  const [confirmStop, setConfirmStop] = useState(false);
  const execution = useQuery({
    queryKey: ['execution-control'], queryFn: portalApi.executionControl,
    refetchInterval: (query) => query.state.data?.state === 'stopping' ? 2000 : false,
  });
  const executionAction = useMutation({
    mutationFn: ({ action, epoch }: { action: 'stop' | 'enable'; epoch: number }) =>
      action === 'stop' ? portalApi.stopAllExecution(epoch) : portalApi.enableExecution(epoch),
    onSettled: async () => {
      setConfirmStop(false);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['execution-control'] }),
        queryClient.invalidateQueries({ queryKey: ['tasks'] }),
      ]);
    },
  });
  const tasks = useInfiniteQuery({
    queryKey: ['tasks'],
    queryFn: ({ pageParam }) => portalApi.tasks(pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    refetchInterval: (query) => query.state.data?.pages.some((page) => page.items.some((task) => activeStatuses.has(task.status))) ? 5000 : false,
  });
  const taskItems = tasks.data?.pages.flatMap((page) => page.items) ?? [];

  return <section>
    <div className="page-heading">
      <div><p className="eyebrow">云端执行</p><h1>任务</h1><p className="muted">查看任务进度、处理需要你决定的步骤，并随时回来查看结果。</p></div>
    </div>
    <section className="task-section execution-control" aria-label="AI 执行控制">
      {execution.isPending && <p className="muted">正在读取 AI 执行状态…</p>}
      {execution.isError && <p className="form-error" role="alert">无法读取 AI 执行状态：{errorDetails(execution.error).message}</p>}
      {execution.data && <div>
        <div className="task-section-heading">
          <div><h2>AI 执行</h2><p className="muted">停止会拦截新任务、取消排队任务，并请求停止运行中的 Agent 任务和 MCP 电脑操作；确认停止后才会解除停止状态。</p></div>
          <span className={`status status-${execution.data.state}`}><span aria-hidden="true" className="status-dot" />
            {execution.data.state === 'enabled' ? '已开启' : execution.data.state === 'stopping' ? '停止中' : '已停止'}
          </span>
        </div>
        {execution.data.state === 'stopping' && <p className="notice notice-info execution-progress" role="status">正在核验停止状态，剩余 {execution.data.pending_task_count} 个执行项。此过程会在后台继续。</p>}
        {execution.data.state === 'disabled' && <p className="muted execution-progress">重新开启只允许新任务运行，不会恢复已取消的任务。</p>}
        {executionAction.isError && <p className="form-error" role="alert">{errorDetails(executionAction.error).message}</p>}
        <div className="host-actions execution-actions">
          {execution.data.state === 'enabled'
            ? <button type="button" className="button button-danger" disabled={executionAction.isPending} onClick={() => setConfirmStop(true)}>停止所有 AI 执行</button>
            : execution.data.state === 'disabled'
              ? <button type="button" className="button button-primary" disabled={executionAction.isPending} onClick={() => executionAction.mutate({ action: 'enable', epoch: execution.data.epoch })}>重新开启</button>
              : <button type="button" className="button button-secondary" disabled>正在停止…</button>}
        </div>
      </div>}
    </section>
    <SchedulePanel />
    {tasks.isPending ? <div className="loading-card" aria-live="polite">正在加载任务…</div>
      : tasks.isError && !taskItems.length ? <div className="error-card" role="alert">{errorDetails(tasks.error).message}<button type="button" className="button button-secondary" onClick={() => void tasks.refetch()}>重试</button></div>
        : !taskItems.length ? <div className="empty-card"><h2>还没有任务</h2><p>通过 API 或 AI 工具创建的云端任务会显示在这里。</p></div>
          : <div className="task-list">
            {tasks.isError && <div className="error-card" role="alert">{errorDetails(tasks.error).message}<button type="button" className="button button-secondary" onClick={() => void (tasks.isFetchNextPageError ? tasks.fetchNextPage() : tasks.refetch())}>重试</button></div>}
            {taskItems.map((task) => <article className="task-card" key={task.id}>
              <div className="task-card-main">
                <div className="task-card-title"><Link to={`/tasks/${encodeURIComponent(task.id)}`}>{task.objective}</Link><span className={`status status-${task.status}`}><span aria-hidden="true" className="status-dot" />{taskStatusLabel[task.status]}</span></div>
                <p className="muted task-card-meta">主机 <span className="mono">{task.host_id}</span> · 最近更新 {formatDate(task.updated_at)}</p>
              </div>
              <Link className="button button-secondary button-small" to={`/tasks/${encodeURIComponent(task.id)}`} aria-label={`查看任务 ${task.objective}`}>查看</Link>
            </article>)}
            {tasks.hasNextPage && !tasks.isError && <button type="button" className="button button-secondary task-load-more" disabled={tasks.isFetchingNextPage} onClick={() => void tasks.fetchNextPage()}>{tasks.isFetchingNextPage ? '正在加载…' : '加载更多任务'}</button>}
          </div>}
    {confirmStop && execution.data?.state === 'enabled' && <ConfirmDialog
      title="停止所有 AI 执行？"
      description="新任务会立即停止派发。排队任务将取消；运行中的 Agent 任务和 MCP 电脑操作会请求运行端停止，并逐项等待确认。主机不会关机，人工接管状态也不会改变。"
      confirmLabel="停止 AI 执行" danger busy={executionAction.isPending}
      onClose={() => setConfirmStop(false)}
      onConfirm={() => { if (execution.data) executionAction.mutate({ action: 'stop', epoch: execution.data.epoch }); }}
    />}
  </section>;
}
