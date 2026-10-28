import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';

type Conversation = Awaited<ReturnType<typeof portalApi.conversations>>['items'][number];
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
type Task = Awaited<ReturnType<typeof portalApi.task>>;

const terminalTaskStatuses = new Set<Task['status']>(['completed', 'failed', 'cancelled']);

function taskStatusText(task: Task, cloudTasksAvailable: boolean | undefined): string {
  if (task.status === 'queued') {
    return cloudTasksAvailable === false
      ? '任务已保存并排队；当前服务未启用云端执行，启用后才会开始运行。'
      : '任务已保存，正在等待云端 Agent 开始。关闭页面不会取消任务。';
  }
  if (task.status === 'running') return '云端 Agent 正在执行。关闭页面不会取消任务。';
  if (task.status === 'waiting_user') return '任务正在等待你的确认或补充信息。';
  if (task.status === 'paused') return '任务已暂停。';
  if (task.status === 'pausing') return '正在安全暂停任务。';
  if (task.status === 'cancelling') return '正在安全停止任务。';
  if (task.status === 'failed') return task.result || '任务执行失败。';
  if (task.status === 'cancelled') return '任务已停止。';
  return task.result || '任务已完成。';
}

export function Assistant() {
  const client = useQueryClient();
  const { notify } = useToast();
  const [conversationID, setConversationID] = useState<string | null>(null);
  const [modelID, setModelID] = useState('');
  const [content, setContent] = useState('');
  const [clarificationAnswer, setClarificationAnswer] = useState('');
  const pendingSend = useRef<{ fingerprint: string; key: string } | null>(null);
  const capabilities = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities });
  const conversations = useQuery({ queryKey: ['conversations'], queryFn: () => portalApi.conversations() });
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections });
  const preferences = useQuery({ queryKey: ['user-preferences'], queryFn: portalApi.preferences });
  const messages = useQuery({
    queryKey: ['conversation-messages', conversationID],
    queryFn: () => portalApi.conversationMessages(conversationID!),
    enabled: Boolean(conversationID),
  });
  const lastMessage = messages.data?.items.at(-1);
  const activeTaskID = lastMessage?.role === 'user' ? lastMessage.task_id : null;
  const currentTask = useQuery({
    queryKey: ['task', activeTaskID],
    queryFn: () => portalApi.task(activeTaskID!),
    enabled: Boolean(activeTaskID),
    refetchInterval: (query) => query.state.data && terminalTaskStatuses.has(query.state.data.status) ? false : 1500,
  });
  const conversationsData = conversations.data?.items ?? [];
  const selected = conversationsData.find((item) => item.id === conversationID);
  const readyModels = models.data?.items.filter((item) => item.checked_at && item.capabilities.text) ?? [];
  const chosenModelID = modelID || preferences.data?.selected_model_id || '';
  const effectiveModelID = readyModels.some((item) => item.id === chosenModelID) ? chosenModelID : '';
  const cloudTasksAvailable = capabilities.data?.supported_features.includes('cloud_tasks');
  const activeTask = currentTask.data;
  const activeTaskStatus = activeTask?.status;
  const taskBusy = Boolean(activeTaskID && (!activeTask || !terminalTaskStatuses.has(activeTask.status)));

  const send = useMutation({
    mutationFn: async (request: { text: string; key: string; conversationID: string | null; modelID: string }) => {
      if (request.conversationID) {
        const result = await portalApi.appendConversationMessage(request.conversationID, {
          model_id: request.modelID, content: request.text,
        }, request.key);
        return result.task;
      }
      return portalApi.submitTask({ model_id: request.modelID, objective: request.text }, request.key);
    },
    onSuccess: async (task, request) => {
      if (pendingSend.current?.key === request.key) pendingSend.current = null;
      setContent('');
      setConversationID(task.conversation_id);
      await Promise.all([
        client.invalidateQueries({ queryKey: ['conversations'] }),
        client.invalidateQueries({ queryKey: ['conversation-messages', task.conversation_id] }),
      ]);
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const respondToApproval = useMutation({
    mutationFn: (request: { taskID: string; approvalID: string; version: number; decision: 'approve' | 'reject' }) =>
      portalApi.respondToTaskApproval(request.taskID, request.approvalID, { version: request.version, decision: request.decision }),
    onSuccess: async (_response, request) => {
      await client.invalidateQueries({ queryKey: ['task', request.taskID] });
      notify(request.decision === 'approve' ? '已批准，任务正在恢复。' : '已拒绝，任务正在处理决定。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const answerClarification = useMutation({
    mutationFn: (request: { taskID: string; clarificationID: string; version: number; answer: string }) =>
      portalApi.answerTaskInput(request.taskID, {
        clarification_id: request.clarificationID, clarification_version: request.version, answer: request.answer,
      }),
    onSuccess: async (_response, request) => {
      setClarificationAnswer('');
      await client.invalidateQueries({ queryKey: ['task', request.taskID] });
      notify('补充信息已提交，任务正在恢复。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = content.trim();
    if (!text || !effectiveModelID || send.isPending || taskBusy) return;
    const fingerprint = JSON.stringify({ conversationID, modelID: effectiveModelID, text });
    if (pendingSend.current?.fingerprint !== fingerprint) pendingSend.current = { fingerprint, key: newIdempotencyKey() };
    send.mutate({ text, key: pendingSend.current.key, conversationID, modelID: effectiveModelID });
  };

  const startNew = () => {
    pendingSend.current = null;
    setConversationID(null);
    setContent('');
  };

  const selectConversation = (item: Conversation) => {
    setConversationID(item.id);
    setContent('');
    pendingSend.current = null;
  };

  const submitClarification = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const request = activeTask?.clarification_request;
    const answer = clarificationAnswer.trim();
    if (!activeTask || !request || !answer || answerClarification.isPending) return;
    answerClarification.mutate({ taskID: activeTask.id, clarificationID: request.id, version: request.version, answer });
  };

  const latestTaskMessage: ConversationMessage | undefined = lastMessage?.role === 'user' ? lastMessage : undefined;

  useEffect(() => {
    if (conversationID && activeTaskStatus && terminalTaskStatuses.has(activeTaskStatus)) {
      void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
    }
  }, [activeTaskStatus, client, conversationID]);

  return <section className="assistant-page">
    <div className="page-heading assistant-heading">
      <div><p className="eyebrow">Morde 云端电脑</p><h1>AI 助手</h1><p className="muted">每条消息都会创建持久云端任务，关闭页面不会取消执行。</p></div>
      <button type="button" className="button button-secondary" onClick={startNew} disabled={send.isPending}>＋ 新对话</button>
    </div>
    {cloudTasksAvailable === false && <div className="notice notice-warning" role="status">
      当前服务尚未启用云端任务执行。提交的任务会保存并保持排队，服务启用后才会开始运行。
    </div>}
    <div className="assistant-layout">
      <aside className="assistant-sidebar" aria-label="最近对话">
        <div className="assistant-sidebar-heading"><h2>最近对话</h2><button type="button" className="icon-button" aria-label="刷新对话" onClick={() => void conversations.refetch()}>↻</button></div>
        {conversations.isPending ? <p className="muted" role="status">正在加载对话…</p> : conversations.isError
          ? <div className="error-inline" role="alert">{errorDetails(conversations.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void conversations.refetch()}>重试</button></div>
          : !conversationsData.length ? <p className="muted assistant-sidebar-empty">还没有对话。发送一条消息即可开始。</p>
            : <div className="conversation-list">{conversationsData.map((item: Conversation) => <button type="button" key={item.id}
              className={`conversation-item ${conversationID === item.id ? 'is-selected' : ''}`} disabled={send.isPending}
              onClick={() => selectConversation(item)}>
              <strong>{item.title || '新对话'}</strong><small>{formatDate(item.updated_at)}</small>
            </button>)}</div>}
      </aside>
      <div className="assistant-main">
        {!conversationID ? <div className="assistant-welcome"><span className="assistant-mark" aria-hidden="true">M</span>
          <h2>今天想聊些什么？</h2><p>发送后会创建云端任务；任务在服务端执行，不依赖此页面保持打开。</p>
        </div> : <div className="conversation-view" aria-live="polite">
          <header className="conversation-view-heading"><div><p className="eyebrow">持久云端任务</p><h2>{selected?.title || '对话'}</h2></div></header>
          {latestTaskMessage && activeTask && <p className="notice notice-info" role="status">{taskStatusText(activeTask, cloudTasksAvailable)}</p>}
          {activeTaskID && currentTask.isPending && <p className="notice notice-info" role="status">正在读取任务状态…</p>}
          {activeTaskID && currentTask.isError && <div className="error-inline" role="alert">无法读取任务状态：{errorDetails(currentTask.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void currentTask.refetch()}>重试</button></div>}
          {activeTask?.pending_request && <section className="notice notice-warning" aria-label="需要批准的操作">
            <h3>{activeTask.pending_request.title}</h3>
            <p>{activeTask.pending_request.object}</p>
            <p>{activeTask.pending_request.content}</p>
            <p>可能影响：{activeTask.pending_request.impact}</p>
            <div className="inline-actions">
              <button type="button" className="button button-primary" disabled={respondToApproval.isPending}
                onClick={() => respondToApproval.mutate({ taskID: activeTask.id, approvalID: activeTask.pending_request!.id, version: activeTask.pending_request!.version, decision: 'approve' })}>批准并继续</button>
              <button type="button" className="button button-secondary" disabled={respondToApproval.isPending}
                onClick={() => respondToApproval.mutate({ taskID: activeTask.id, approvalID: activeTask.pending_request!.id, version: activeTask.pending_request!.version, decision: 'reject' })}>拒绝</button>
            </div>
          </section>}
          {activeTask?.clarification_request && <form className="notice notice-warning" aria-label="补充任务信息" onSubmit={submitClarification}>
            <h3>任务需要补充信息</h3>
            <p>{activeTask.clarification_request.question}</p>
            <label htmlFor="task-clarification-answer">你的回答</label>
            <textarea id="task-clarification-answer" value={clarificationAnswer} maxLength={16384} required
              onChange={(event) => setClarificationAnswer(event.target.value)} disabled={answerClarification.isPending} />
            <button type="submit" className="button button-primary" disabled={!clarificationAnswer.trim() || answerClarification.isPending}>
              {answerClarification.isPending ? '正在提交…' : '提交并继续'}
            </button>
          </form>}
          {messages.isPending ? <div className="loading-card" role="status">正在加载消息…</div>
            : messages.isError ? <div className="error-card" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-secondary" onClick={() => void messages.refetch()}>重试</button></div>
              : <div className="message-list">{messages.data.items.map((message) => <article className={`chat-message chat-message-${message.role}`} key={message.id}>
                <strong className="chat-message-author">{message.role === 'user' ? '你' : 'AI 助手'}</strong><p>{message.content}</p><small>{formatDate(message.created_at)}</small>
              </article>)}</div>}
        </div>}
        <section className="assistant-composer" aria-label="发送云端任务消息">
          {models.isError && <div className="error-inline" role="alert">无法读取模型：{errorDetails(models.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void models.refetch()}>重试</button></div>}
          {!models.isPending && !models.isError && !readyModels.length && <div className="notice notice-warning assistant-setup-notice" role="status">还没有可用的 AI 模型。<Link to="/connections">检查模型设置</Link></div>}
          <form onSubmit={submit}>
            <label className="sr-only" htmlFor="assistant-content">消息内容</label>
            <textarea id="assistant-content" value={content} onChange={(event) => setContent(event.target.value)} maxLength={32768}
              placeholder="输入你想问的问题…" disabled={send.isPending || taskBusy || Boolean(conversationID && messages.isPending)} />
            <div className="assistant-composer-footer"><div className="assistant-selectors"><label><span>AI 模型</span><select aria-label="选择 AI 模型"
              value={effectiveModelID} onChange={(event) => setModelID(event.target.value)} disabled={models.isPending || send.isPending || taskBusy}>
              <option value="">选择模型</option>{readyModels.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.model_name}</option>)}
            </select></label></div>
              <button className="button button-primary assistant-send" type="submit" disabled={!content.trim() || !effectiveModelID || send.isPending || taskBusy || Boolean(conversationID && messages.isPending)}>{send.isPending ? '正在提交…' : taskBusy ? '任务进行中' : '发送'} <span aria-hidden="true">↑</span></button>
            </div>
          </form>
          <p className="composer-hint">任务状态和回复会保存到当前对话；你可以在任务完成后切换模型继续。</p>
        </section>
      </div>
    </div>
  </section>;
}
