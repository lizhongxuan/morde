import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useParams } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { components } from '../api/schema';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { isActiveTaskStatus, taskStatusLabel } from './Tasks';

type Task = Awaited<ReturnType<typeof portalApi.task>>;
type TaskAction = components['schemas']['TaskActionRequest'];
type ApprovalRequest = NonNullable<Task['pending_request']>;

const eventTypes = ['task.status', 'task.step', 'task.operation', 'task.batch_item', 'task.artifact', 'task.waiting_user', 'task.response', 'task.result', 'task.snapshot_required'] as const;
const stepStatusLabel: Record<Task['steps'][number]['status'], string> = {
  queued: '等待中', running: '进行中', waiting_user: '等待你处理', succeeded: '已完成', failed: '失败', unknown: '待核实',
};

export function TaskDetail() {
  const { id = '' } = useParams();
  const queryClient = useQueryClient();
  const { notify } = useToast();
  const [confirmCancel, setConfirmCancel] = useState(false);
  const [clarificationAnswer, setClarificationAnswer] = useState('');
  const actionKeys = useRef(new Map<string, string>());
  const taskQuery = useQuery({
    queryKey: ['task', id], queryFn: () => portalApi.task(id), enabled: Boolean(id),
    refetchInterval: (query) => query.state.data && isActiveTaskStatus(query.state.data.status) ? 15000 : false,
  });
  const task = taskQuery.data;
  useEffect(() => { actionKeys.current.clear(); }, [id]);
  const action = useMutation({
    mutationFn: ({ body, key }: { body: TaskAction; key: string }) => portalApi.taskAction(id, body, key),
    onSuccess: (_result, { body, key }) => {
      const fingerprint = JSON.stringify(body);
      if (actionKeys.current.get(fingerprint) === key) actionKeys.current.delete(fingerprint);
      setConfirmCancel(false);
      const message = body.action === 'respond' ? '已提交审批决定。'
        : body.action === 'answer_clarification' ? '已提交补充信息，任务已重新排队。'
          : body.action === 'pause' ? '已提交暂停请求。'
          : body.action === 'resume' ? '任务已重新排队。'
            : body.action === 'cancel' ? '已提交取消请求。'
              : body.action === 'retry_batch_item' ? '已提交批次重试。' : '已提交操作。';
      if (body.action === 'answer_clarification') setClarificationAnswer('');
      notify(message, 'success');
      void queryClient.invalidateQueries({ queryKey: ['task', id] });
      void queryClient.invalidateQueries({ queryKey: ['tasks'] });
    },
    onError: (error) => {
      notify(errorDetails(error).message, 'error');
      void queryClient.invalidateQueries({ queryKey: ['task', id] });
    },
  });
  const submitAction = (body: TaskAction) => {
    const fingerprint = JSON.stringify(body);
    let key = actionKeys.current.get(fingerprint);
    if (!key) {
      key = newIdempotencyKey();
      actionKeys.current.set(fingerprint, key);
    }
    action.mutate({ body, key });
  };

  const shouldStreamTaskEvents = Boolean(task && isActiveTaskStatus(task.status));
  useEffect(() => {
    if (!shouldStreamTaskEvents || typeof EventSource === 'undefined') return;
    const stream = new EventSource(`/api/tasks/${encodeURIComponent(id)}/events`);
    const refresh = () => { void queryClient.invalidateQueries({ queryKey: ['task', id] }); };
    for (const type of eventTypes) stream.addEventListener(type, refresh);
    return () => { for (const type of eventTypes) stream.removeEventListener(type, refresh); stream.close(); };
  }, [id, queryClient, shouldStreamTaskEvents]);

  if (!id) return <div className="error-card" role="alert">任务编号无效。<Link to="/tasks">返回任务列表</Link></div>;
  if (taskQuery.isPending) return <div className="loading-card" aria-live="polite">正在加载任务…</div>;
  if (taskQuery.isError || !task) return <section><Link className="back-link" to="/tasks">← 返回任务列表</Link><div className="error-card" role="alert">{errorDetails(taskQuery.error).message}<button type="button" className="button button-secondary" onClick={() => void taskQuery.refetch()}>重试</button></div></section>;

  const pendingApproval = task.pending_request;
  const pendingClarification = task.clarification_request;
  const canPause = task.status === 'queued' || task.status === 'running';
  const canCancel = task.status === 'queued' || task.status === 'running' || task.status === 'waiting_user' || task.status === 'paused';
  const canResume = task.status === 'paused';
  const pauseTask = () => {
    if (task.status === 'queued' || task.status === 'running') submitAction({ action: 'pause', expected_status: task.status });
  };
  const cancelTask = () => {
    if (task.status === 'queued' || task.status === 'running' || task.status === 'waiting_user' || task.status === 'paused') submitAction({ action: 'cancel', expected_status: task.status });
  };

  return <section>
    <Link className="back-link" to="/tasks">← 返回任务列表</Link>
    <div className="page-heading task-detail-heading">
      <div><p className="eyebrow">云端任务</p><h1>{task.objective}</h1><p className="muted">任务 ID <span className="mono">{task.id}</span></p></div>
      <span className={`status status-${task.status}`}><span aria-hidden="true" className="status-dot" />{taskStatusLabel[task.status]}</span>
    </div>

    <div className="task-layout">
      <div className="task-main-column">
        {pendingApproval && <ApprovalPanel request={pendingApproval} busy={action.isPending} onRespond={(decision) => submitAction({
          action: 'respond', pending_id: pendingApproval.id, pending_version: pendingApproval.version, decision,
        })} />}
        {pendingClarification && <ClarificationPanel request={pendingClarification} answer={clarificationAnswer} busy={action.isPending}
          onAnswerChange={setClarificationAnswer}
          onSubmit={() => submitAction({ action: 'answer_clarification', clarification_id: pendingClarification.id, clarification_version: pendingClarification.version, answer: clarificationAnswer })} />}
        {task.status === 'waiting_user' && !pendingApproval && !pendingClarification && <div className="task-notice" role="status">
          <strong>任务正在等待处理</strong><p>当前没有可显示的待处理请求。请刷新任务状态后再继续。</p>
          <button type="button" className="button button-secondary button-small" onClick={() => void taskQuery.refetch()}>刷新状态</button>
        </div>}

        <section className="task-section">
          <div className="task-section-heading"><div><h2>执行进度</h2><p className="muted">任务活动会自动更新。</p></div>
            <button type="button" className="button button-quiet button-small" onClick={() => void taskQuery.refetch()} disabled={taskQuery.isFetching}>刷新</button>
          </div>
          {task.steps.length ? <ol className="task-steps">{task.steps.map((step) => <li className="task-step" key={step.id}>
            <span className={`task-step-marker task-step-${step.status}`} aria-hidden="true" />
            <div><div className="task-step-title"><strong>{step.title}</strong><span className="muted">{stepStatusLabel[step.status]}</span></div>
              {step.summary && <p>{step.summary}</p>}<small className="muted">{formatDate(step.created_at)}</small></div>
          </li>)}</ol> : <div className="empty-card compact"><p>任务开始后，执行步骤会显示在这里。</p></div>}
        </section>

        {task.batch_items.length > 0 && <section className="task-section">
          <h2>批次进度</h2><div className="task-batch-list">{task.batch_items.map((item) => <article className="task-batch-item" key={item.id}>
            <div><strong>{item.key}</strong><p>{item.result || item.error || '等待执行'}</p></div>
            <span className={`status status-${item.status}`}>{item.status === 'succeeded' ? '已完成' : item.status === 'failed' ? '失败' : item.status === 'running' ? '进行中' : item.status === 'unknown' ? '待核实' : '排队中'}</span>
            {item.status === 'failed' && <button type="button" className="button button-secondary button-small" disabled={action.isPending} onClick={() => submitAction({ action: 'retry_batch_item', batch_item_id: item.id })}>重试</button>}
          </article>)}</div>
        </section>}

        {task.result && <section className="task-section"><h2>任务结果</h2><div className="task-result">{task.result}</div></section>}
        {task.artifacts.length > 0 && <section className="task-section"><h2>产物</h2><ul className="task-artifacts">{task.artifacts.map((artifact) => <li key={artifact.id}><strong>{artifact.label}</strong><span className="mono">{artifact.path}</span></li>)}</ul></section>}
      </div>

      <aside className="task-side-column">
        <section className="task-section task-facts">
          <h2>任务信息</h2>
          <dl><div><dt>主机</dt><dd className="mono">{task.host_id}</dd></div><div><dt>模型</dt><dd>{task.model_id}</dd></div>
            <div><dt>创建时间</dt><dd>{formatDate(task.created_at)}</dd></div><div><dt>最近更新</dt><dd>{formatDate(task.updated_at)}</dd></div></dl>
        </section>
        <section className="task-section task-facts">
          <h2>用量</h2>
          <dl><div><dt>运行时间</dt><dd>{task.usage.active_seconds} / {task.budget.active_seconds} 秒</dd></div>
            <div><dt>工具调用</dt><dd>{task.usage.tool_calls} / {task.budget.tool_calls}</dd></div>
            <div><dt>模型 Token</dt><dd>{task.usage.model_tokens === null ? '未知' : task.usage.model_tokens.toLocaleString()} / {task.budget.model_tokens.toLocaleString()}</dd></div></dl>
        </section>
        <section className="task-section task-facts">
          <h2>任务控制</h2>
          <div className="task-control-actions">
            {canPause && <button type="button" className="button button-secondary" disabled={action.isPending} onClick={pauseTask}>暂停任务</button>}
            {canResume && <button type="button" className="button button-primary" disabled={action.isPending} onClick={() => submitAction({ action: 'resume', expected_status: 'paused' })}>继续任务</button>}
            {canCancel && <button type="button" className="button button-danger-quiet" disabled={action.isPending} onClick={() => setConfirmCancel(true)}>取消任务</button>}
            {!canPause && !canResume && !canCancel && <p className="muted">任务已结束。</p>}
          </div>
          {task.stop_request?.status === 'pending' && <p className="notice notice-warning task-stop-notice">正在确认任务中的外部操作已停止。未确认的操作：{task.stop_request.unsettled_operation_ids.length ? task.stop_request.unsettled_operation_ids.join(', ') : '无'}</p>}
        </section>
      </aside>
    </div>
    {confirmCancel && <ConfirmDialog title="取消此任务？" description="任务会停止派发新操作。已开始的操作需要运行端确认停止，外部副作用不会自动撤销。" confirmLabel="取消任务" danger busy={action.isPending} onClose={() => setConfirmCancel(false)} onConfirm={cancelTask} />}
  </section>;
}

function ApprovalPanel({ request, busy, onRespond }: { request: ApprovalRequest; busy: boolean; onRespond: (decision: 'approve' | 'reject') => void }) {
  return <section className="pending-card" aria-labelledby="pending-title">
    <div className="pending-card-top"><span className="pending-icon" aria-hidden="true">?</span><div><p className="eyebrow">需要你决定</p><h2 id="pending-title">{request.title}</h2></div></div>
    <dl className="pending-facts"><div><dt>对象</dt><dd>{request.object}</dd></div><div><dt>内容</dt><dd>{request.content}</dd></div><div><dt>影响</dt><dd>{request.impact}</dd></div></dl>
    <p className="muted pending-version">请求版本 {request.version}</p>
    <div className="task-control-actions"><button type="button" className="button button-secondary" disabled={busy} onClick={() => onRespond('reject')}>拒绝</button><button type="button" className="button button-primary" disabled={busy} onClick={() => onRespond('approve')}>{busy ? '提交中…' : '批准并继续'}</button></div>
  </section>;
}

type ClarificationRequest = NonNullable<Task['clarification_request']>;
const maxClarificationAnswerLength = 16384;

function ClarificationPanel({ request, answer, busy, onAnswerChange, onSubmit }: {
  request: ClarificationRequest;
  answer: string;
  busy: boolean;
  onAnswerChange: (value: string) => void;
  onSubmit: () => void;
}) {
  const characterCount = Array.from(answer).length;
  const hasNul = answer.includes('\0');
  const valid = answer.trim().length > 0 && !hasNul && characterCount <= maxClarificationAnswerLength;
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (valid && !busy) onSubmit();
  };
  return <section className="clarification-card" aria-labelledby="clarification-title">
    <div className="clarification-card-top"><span className="clarification-icon" aria-hidden="true">✎</span><div><p className="eyebrow">需要你补充信息</p><h2 id="clarification-title">任务等你回答</h2></div></div>
    <p className="clarification-question">{request.question}</p>
    <p className="muted clarification-version">请求版本 {request.version}</p>
    <form onSubmit={submit}>
      <label htmlFor="clarification-answer">你的回答</label>
      <textarea id="clarification-answer" value={answer} maxLength={maxClarificationAnswerLength * 2} rows={5} disabled={busy}
        aria-describedby="clarification-answer-help" placeholder="输入补充信息…" onChange={(event) => onAnswerChange(event.target.value)} />
      <div id="clarification-answer-help" className="clarification-form-foot"><span className={characterCount > maxClarificationAnswerLength ? 'text-danger' : 'muted'}>{characterCount.toLocaleString()} / {maxClarificationAnswerLength.toLocaleString()}</span>
        {hasNul && <span className="text-danger" role="alert">回答不能包含空字符。</span>}
      </div>
      <div className="clarification-submit"><button type="submit" className="button button-primary" disabled={busy || !valid}>{busy ? '提交中…' : '提交回答'}</button></div>
    </form>
  </section>;
}
