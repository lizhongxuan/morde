import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { formatDate, hostStateLabel } from '../app/format';
import { useToast } from '../app/toast';

type Conversation = Awaited<ReturnType<typeof portalApi.conversations>>['items'][number];
type ConversationMessage = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
type Task = Awaited<ReturnType<typeof portalApi.task>>;
type Host = Awaited<ReturnType<typeof portalApi.hosts>>['items'][number];
type WorkspaceBinding = Host['workspaces'][number];
type AssistantDisplayMessage = ConversationMessage & {
  snapshotStatus?: 'streaming' | 'completed' | 'interrupted';
};

export function assistantRecentConversationKey(serviceInstanceID: string, userID: string): string {
  return `morde.assistant.recentConversation.v1:${serviceInstanceID}:${userID}`;
}

function readRecentConversation(key: string): string | null {
  try { return window.localStorage.getItem(key); } catch { return null; }
}

function writeRecentConversation(key: string, conversationID: string | null): void {
  try {
    if (conversationID) window.localStorage.setItem(key, conversationID);
    else window.localStorage.removeItem(key);
  } catch { /* the conversation remains usable for this page session */ }
}

export function mergeTaskAssistantMessage(items: ConversationMessage[], task: Task | undefined): AssistantDisplayMessage[] {
  const messages: AssistantDisplayMessage[] = [...items];
  const snapshot = task?.assistant_message;
  if (!snapshot || snapshot.role !== 'assistant') return messages;
  const existingIndex = messages.findIndex((message) => message.id === snapshot.id);
  if (existingIndex >= 0) {
    // The conversation endpoint only exposes committed messages. It wins when a Task
    // snapshot was read just before the final message transaction committed.
    const existing = messages[existingIndex];
    if (existing) messages[existingIndex] = { ...existing, snapshotStatus: 'completed' };
  } else {
    messages.push({ ...snapshot, snapshotStatus: snapshot.status });
  }
  return messages;
}

const terminalTaskStatuses = new Set<Task['status']>(['completed', 'failed', 'cancelled']);
const controlStateLabels: Record<Host['control_state'], string> = {
  ai: '云端 AI 控制', taking_over: '正在接管', user: '人工控制', returning: '正在交还 AI',
};
const runtimeStateLabels: Record<Awaited<ReturnType<typeof portalApi.execution>>['runtime']['state'], string> = {
  not_started: '未启动', starting: '启动中', running: '运行中', stopping: '停止中', stopped: '已停止', failed: '启动失败',
};
const executionStateLabels: Record<Awaited<ReturnType<typeof portalApi.execution>>['state'], string> = {
  enabled: '任务接收已启用', stopping: '正在停止任务接收', disabled: '任务接收已停用',
};

function workspaceBindingReady(binding: WorkspaceBinding): boolean {
  return binding.state === 'active' && binding.workspace.state === 'ready' && binding.sync_state === 'healthy' &&
    binding.applied_revision === binding.desired_revision && binding.applied_revision === binding.workspace.revision;
}

function taskTargetText(task: Task, hosts: Host[]): string {
  const runtime = task.execution_location === 'server' ? '云端 Agent Runtime' : '旧客户端 Runtime';
  const host = task.host_id ? hosts.find((item) => item.id === task.host_id) : undefined;
  const computer = task.host_id ? host ? `${host.name || host.id}（${hostStateLabel[host.state] || host.state}）` : `主机 ${task.host_id}（当前不可访问）` : '无电脑目标';
  const binding = task.workspace_id ? host?.workspaces.find((item) => item.workspace.id === task.workspace_id) : undefined;
  const workspace = task.workspace_id ? `${binding?.workspace.name || task.workspace_id}${task.input_revision === undefined ? '' : ` v${task.input_revision}`}` : '无 Workspace';
  return `${runtime} · ${computer} · ${workspace}`;
}

function taskStatusText(task: Task, cloudTasksAvailable: boolean | undefined): string {
  if (task.status === 'queued') {
    return cloudTasksAvailable === false
      ? '任务已保存并排队；当前服务未启用云端执行，启用后才会开始运行。'
      : '任务已保存，正在等待云端 Agent 开始。关闭页面不会取消任务。';
  }
  if (task.status === 'running') return '云端 Agent 正在执行。关闭页面不会取消任务。';
  if (task.status === 'waiting_user' && task.waiting_reason === 'workspace_host_sync') return '任务等待目标云端电脑重新就绪并同步 Workspace；确认版本已应用后才会完成。';
  if (task.status === 'waiting_user') return '任务正在等待你的确认或补充信息。';
  if (task.status === 'paused') return '任务已暂停。';
  if (task.status === 'pausing') return '正在安全暂停任务。';
  if (task.status === 'cancelling') {
    const unsettled = task.stop_request?.unsettled_operation_ids.length ?? 0;
    if (unsettled > 0) return `停止请求已受理，仍有 ${unsettled} 个操作结果待确认。`;
    if (task.stop_request?.status === 'pending') return '正在等待服务端确认停止结果。';
    if (task.stop_request?.status === 'completed') return '服务端已处理停止请求，正在等待最终任务状态。';
    return '停止状态待确认，正在重新读取服务端任务。';
  }
  if (task.status === 'failed') return task.result || '任务执行失败。';
  if (task.status === 'cancelled') return '服务端已确认任务停止。';
  return task.result || '任务已完成。';
}

export function Assistant() {
  const client = useQueryClient();
  const { notify } = useToast();
  const { user } = useAuth();
  const [conversationSelection, setConversationSelection] = useState<{ scope: string; id: string | null } | null>(null);
  const [hostSelection, setHostSelection] = useState<string | null>(null);
  const [workspaceSelection, setWorkspaceSelection] = useState('');
  const [modelID, setModelID] = useState('');
  const [content, setContent] = useState('');
  const [clarificationAnswer, setClarificationAnswer] = useState('');
  const pendingSend = useRef<{ fingerprint: string; key: string } | null>(null);
  const pendingTaskActions = useRef(new Map<string, { fingerprint: string; key: string }>());
  const capabilities = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities });
  const conversations = useQuery({ queryKey: ['conversations'], queryFn: () => portalApi.conversations() });
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections });
  const preferences = useQuery({ queryKey: ['user-preferences'], queryFn: portalApi.preferences });
  const hosts = useQuery({
    queryKey: ['assistant-hosts'], queryFn: portalApi.hosts,
    refetchInterval: (query) => query.state.data?.items.some((host) => ['creating', 'stopping', 'deleting'].includes(host.state)) ? 3000 : 30000,
  });
  const execution = useQuery({ queryKey: ['execution-control'], queryFn: portalApi.execution, refetchInterval: 15000 });
  const conversationsData = conversations.data?.items ?? [];
  const recentConversationKey = user && capabilities.data?.service_instance_id
    ? assistantRecentConversationKey(capabilities.data.service_instance_id, user.id) : null;
  const recentConversation = useQuery({
    queryKey: ['assistant-recent-conversation', recentConversationKey],
    queryFn: () => recentConversationKey ? readRecentConversation(recentConversationKey) : null,
    enabled: Boolean(recentConversationKey),
    staleTime: Infinity,
  });
  const conversationScope = `${window.location.origin}:${user?.id ?? 'anonymous'}`;
  const scopedConversationSelection = conversationSelection?.scope === conversationScope ? conversationSelection : null;
  const restoredConversationID = recentConversation.data && conversationsData.some((item) => item.id === recentConversation.data)
    ? recentConversation.data : null;
  const conversationID = scopedConversationSelection ? scopedConversationSelection.id : restoredConversationID;
  const setConversationID = (id: string | null) => setConversationSelection({ scope: conversationScope, id });
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
  const selected = conversationsData.find((item) => item.id === conversationID);
  const readyModels = models.data?.items.filter((item) => item.checked_at && item.capabilities.text) ?? [];
  const chosenModelID = modelID || preferences.data?.selected_model_id || '';
  const effectiveModelID = readyModels.some((item) => item.id === chosenModelID) ? chosenModelID : '';
  const cloudTasksAvailable = capabilities.isSuccess
    ? capabilities.data.supported_features.includes('cloud_tasks')
    : undefined;
  // Older services used cloud_tasks to mean that the Task API was present too.
  const taskAPIAvailable = capabilities.isSuccess && (
    capabilities.data.supported_features.includes('task_api') || cloudTasksAvailable === true
  );
  const preferredHostID = hostSelection ?? preferences.data?.default_host_id ?? '';
  const hostItems = hosts.data?.items ?? [];
  const selectedHost = hostItems.find((host) => host.id === preferredHostID);
  const selectedHostID = selectedHost?.id ?? '';
  const hostIsReady = selectedHost?.state === 'running' && selectedHost.control_state === 'ai';
  const workspaceBindings = selectedHost?.workspaces ?? [];
  const selectedBinding = workspaceBindings.find((binding) => binding.workspace.id === workspaceSelection);
  const targetUnavailable = Boolean(preferredHostID && hosts.isPending) || Boolean(hostSelection && !selectedHost) ||
    Boolean(selectedHost && !hostIsReady) || Boolean(workspaceSelection && !selectedBinding) ||
    Boolean(selectedBinding && !workspaceBindingReady(selectedBinding));
  const activeTask = currentTask.data;
  const displayMessages = mergeTaskAssistantMessage(messages.data?.items ?? [], activeTask);
  const activeTaskStatus = activeTask?.status;
  const taskBusy = Boolean(activeTaskID && (!activeTask || !terminalTaskStatuses.has(activeTask.status)));
  const taskActionKey = (scope: string, fingerprint: string) => {
    const previous = pendingTaskActions.current.get(scope);
    if (previous?.fingerprint === fingerprint) return previous.key;
    const key = newIdempotencyKey();
    pendingTaskActions.current.set(scope, { fingerprint, key });
    return key;
  };
  const clearTaskActionKey = (scope: string, key: string) => {
    if (pendingTaskActions.current.get(scope)?.key === key) pendingTaskActions.current.delete(scope);
  };
  const handleTaskActionError = async (error: unknown, taskID: string) => {
    const details = errorDetails(error);
    if (details.code === 'conflict') {
      await client.invalidateQueries({ queryKey: ['task', taskID] });
      notify('任务状态已变化，已刷新最新状态。请核对后再操作。', 'error');
      return;
    }
    notify(details.message, 'error');
  };

  const send = useMutation({
    mutationFn: async (request: { text: string; key: string; conversationID: string | null; modelID: string; hostID?: string; workspaceID?: string; inputRevision?: number }) => {
      const hostTarget = request.hostID ? { host_id: request.hostID } : {};
      const workspaceTarget = request.workspaceID && request.inputRevision !== undefined
        ? { workspace_id: request.workspaceID, input_revision: request.inputRevision }
        : {};
      if (request.conversationID) {
        const result = await portalApi.appendConversationMessage(request.conversationID, {
          model_id: request.modelID, content: request.text, ...hostTarget, ...workspaceTarget,
        }, request.key);
        return result.task;
      }
      return portalApi.submitTask({ model_id: request.modelID, objective: request.text, ...hostTarget, ...workspaceTarget }, request.key);
    },
    onSuccess: async (task, request) => {
      if (pendingSend.current?.key === request.key) pendingSend.current = null;
      setContent('');
      setConversationID(task.conversation_id);
      if (recentConversationKey) {
        writeRecentConversation(recentConversationKey, task.conversation_id);
        client.setQueryData(['assistant-recent-conversation', recentConversationKey], task.conversation_id);
      }
      await Promise.all([
        client.invalidateQueries({ queryKey: ['conversations'] }),
        client.invalidateQueries({ queryKey: ['conversation-messages', task.conversation_id] }),
      ]);
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const respondToApproval = useMutation({
    mutationFn: (request: { taskID: string; approvalID: string; version: number; decision: 'approve' | 'reject'; actionScope: string; key: string }) =>
      portalApi.respondToTaskApproval(request.taskID, request.approvalID,
        { version: request.version, decision: request.decision }, request.key),
    onSuccess: async (_response, request) => {
      clearTaskActionKey(request.actionScope, request.key);
      await client.invalidateQueries({ queryKey: ['task', request.taskID] });
      notify(request.decision === 'approve' ? '已批准，任务正在恢复。' : '已拒绝，任务正在处理决定。', 'success');
    },
    onError: (error, request) => handleTaskActionError(error, request.taskID),
  });
  const answerClarification = useMutation({
    mutationFn: (request: { taskID: string; clarificationID: string; version: number; answer: string; actionScope: string; key: string }) =>
      portalApi.answerTaskInput(request.taskID, {
        clarification_id: request.clarificationID, clarification_version: request.version, answer: request.answer,
      }, request.key),
    onSuccess: async (_response, request) => {
      clearTaskActionKey(request.actionScope, request.key);
      setClarificationAnswer('');
      await client.invalidateQueries({ queryKey: ['task', request.taskID] });
      notify('补充信息已提交，任务正在恢复。', 'success');
    },
    onError: (error, request) => handleTaskActionError(error, request.taskID),
  });
  const stopTask = useMutation({
    mutationFn: (request: { taskID: string; expectedStatus: Task['status']; actionScope: string; key: string }) =>
      portalApi.taskAction(request.taskID, { action: 'cancel', expected_status: request.expectedStatus }, request.key),
    onSuccess: async (response, request) => {
      clearTaskActionKey(request.actionScope, request.key);
      await client.invalidateQueries({ queryKey: ['task', request.taskID] });
      if (response.status === 'cancelled' && response.unsettled_operation_ids.length === 0) {
        notify('服务端已确认任务停止。', 'success');
      } else if (response.unsettled_operation_ids.length > 0) {
        notify('停止请求已受理，等待任务状态更新。', 'info');
      } else {
        notify('停止请求已受理，仍需等待服务端确认任务状态。', 'info');
      }
    },
    onError: (error, request) => handleTaskActionError(error, request.taskID),
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = content.trim();
    if (!text || !effectiveModelID || !taskAPIAvailable || send.isPending || taskBusy || targetUnavailable) return;
    const target = {
      hostID: selectedHostID,
      workspaceID: selectedBinding?.workspace.id ?? '',
      inputRevision: selectedBinding?.workspace.revision,
    };
    const fingerprint = JSON.stringify({ conversationID, modelID: effectiveModelID, text, target });
    if (pendingSend.current?.fingerprint !== fingerprint) pendingSend.current = { fingerprint, key: newIdempotencyKey() };
    send.mutate({ text, key: pendingSend.current.key, conversationID, modelID: effectiveModelID,
      ...(target.hostID ? { hostID: target.hostID } : {}),
      ...(target.workspaceID && target.inputRevision !== undefined ? { workspaceID: target.workspaceID, inputRevision: target.inputRevision } : {}),
    });
  };

  const startNew = () => {
    pendingSend.current = null;
    setConversationID(null);
    setContent('');
    if (recentConversationKey) {
      writeRecentConversation(recentConversationKey, null);
      client.setQueryData(['assistant-recent-conversation', recentConversationKey], null);
    }
  };

  const selectConversation = (item: Conversation) => {
    setConversationID(item.id);
    setContent('');
    pendingSend.current = null;
    if (recentConversationKey) {
      writeRecentConversation(recentConversationKey, item.id);
      client.setQueryData(['assistant-recent-conversation', recentConversationKey], item.id);
    }
  };

  const submitClarification = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const request = activeTask?.clarification_request;
    const answer = clarificationAnswer.trim();
    if (!activeTask || !request || !answer || answerClarification.isPending) return;
    const actionScope = `clarification:${activeTask.id}:${request.id}`;
    const fingerprint = JSON.stringify({ version: request.version, answer });
    answerClarification.mutate({ taskID: activeTask.id, clarificationID: request.id, version: request.version,
      answer, actionScope, key: taskActionKey(actionScope, fingerprint) });
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
    {capabilities.isPending && <p className="notice notice-info" role="status">正在检查服务能力…</p>}
    {capabilities.isError && <div className="error-inline" role="alert">无法读取服务能力：{errorDetails(capabilities.error).message}<button type="button" className="button button-small button-secondary" aria-label="重试服务能力" onClick={() => void capabilities.refetch()}>重试</button></div>}
    {capabilities.isSuccess && !taskAPIAvailable && <div className="notice notice-warning" role="status">
      当前服务不支持云端任务 API，无法提交任务。请更新服务后重试。
    </div>}
    {taskAPIAvailable && cloudTasksAvailable === false && <div className="notice notice-warning" role="status">
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
          {latestTaskMessage && activeTask && <p className="assistant-task-target" role="status">当前 Task 的目标已固定：{taskTargetText(activeTask, hostItems)}。下面的目标选择只影响下一条消息。</p>}
          {activeTaskID && currentTask.isPending && <p className="notice notice-info" role="status">正在读取任务状态…</p>}
          {activeTaskID && currentTask.isError && <div className="error-inline" role="alert">无法读取任务状态：{errorDetails(currentTask.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void currentTask.refetch()}>重试</button></div>}
          {activeTask && ['queued', 'running', 'waiting_user', 'paused'].includes(activeTask.status) && <div className="inline-actions">
            <button type="button" className="button button-secondary" disabled={stopTask.isPending}
              onClick={() => {
                const actionScope = `stop:${activeTask.id}`;
                const fingerprint = JSON.stringify({ action: 'cancel', expected_status: activeTask.status });
                stopTask.mutate({ taskID: activeTask.id, expectedStatus: activeTask.status, actionScope,
                  key: taskActionKey(actionScope, fingerprint) });
              }}>{stopTask.isPending ? '正在提交停止请求…' : '停止任务'}</button>
          </div>}
          {activeTask?.pending_request && <section className="notice notice-warning" aria-label="需要批准的操作">
            <h3>{activeTask.pending_request.title}</h3>
            <p>{activeTask.pending_request.object}</p>
            <p>{activeTask.pending_request.content}</p>
            <p>可能影响：{activeTask.pending_request.impact}</p>
            <div className="inline-actions">
              <button type="button" className="button button-primary" disabled={respondToApproval.isPending}
                onClick={() => {
                  const request = activeTask.pending_request!;
                  const actionScope = `approval:${activeTask.id}:${request.id}`;
                  const fingerprint = JSON.stringify({ version: request.version, decision: 'approve' });
                  respondToApproval.mutate({ taskID: activeTask.id, approvalID: request.id, version: request.version,
                    decision: 'approve', actionScope, key: taskActionKey(actionScope, fingerprint) });
                }}>批准并继续</button>
              <button type="button" className="button button-secondary" disabled={respondToApproval.isPending}
                onClick={() => {
                  const request = activeTask.pending_request!;
                  const actionScope = `approval:${activeTask.id}:${request.id}`;
                  const fingerprint = JSON.stringify({ version: request.version, decision: 'reject' });
                  respondToApproval.mutate({ taskID: activeTask.id, approvalID: request.id, version: request.version,
                    decision: 'reject', actionScope, key: taskActionKey(actionScope, fingerprint) });
                }}>拒绝</button>
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
          {messages.isPending && displayMessages.length === 0 ? <div className="loading-card" role="status">正在加载消息…</div>
            : messages.isError && displayMessages.length === 0 ? <div className="error-card" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-secondary" onClick={() => void messages.refetch()}>重试</button></div>
              : <div className="message-list">
                {messages.isError && <div className="error-inline" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void messages.refetch()}>重试</button></div>}
                {displayMessages.map((message) => <article className={`chat-message chat-message-${message.role}`} key={message.id}>
                  <strong className="chat-message-author">{message.role === 'user' ? '你' : 'AI 助手'}</strong><p>{message.content}</p>
                  {message.snapshotStatus === 'streaming' && <small role="status">正在生成…</small>}
                  {message.snapshotStatus === 'interrupted' && <small role="status">回复中断，以下为已恢复内容</small>}
                  <small>{formatDate(message.created_at)}</small>
                </article>)}
              </div>}
        </div>}
        <section className="assistant-composer" aria-label="发送云端任务消息">
          {models.isError && <div className="error-inline" role="alert">无法读取模型：{errorDetails(models.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void models.refetch()}>重试</button></div>}
          {!models.isPending && !models.isError && !readyModels.length && <div className="notice notice-warning assistant-setup-notice" role="status">还没有可用的 AI 模型。<Link to="/connections">检查模型设置</Link></div>}
          {execution.isPending && <p className="notice notice-info assistant-runtime-status" role="status">正在读取云端 Agent Runtime 状态…电脑状态会单独显示在目标选择中。</p>}
          {execution.data && <p className="notice notice-info assistant-runtime-status" role="status">云端 Agent Runtime：{runtimeStateLabels[execution.data.runtime.state]}；{executionStateLabels[execution.data.state]}；运行中 {execution.data.runtime.active_task_count}/{execution.data.runtime.max_concurrent_tasks} 个任务。目标电脑状态单独显示。</p>}
          {execution.isError && <div className="error-inline" role="alert">无法读取云端 Agent Runtime 状态：{errorDetails(execution.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void execution.refetch()}>重试</button></div>}
          {hosts.isError && <div className="error-inline" role="alert">无法读取目标电脑：{errorDetails(hosts.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void hosts.refetch()}>重试</button></div>}
          {preferredHostID && !selectedHost && !hosts.isPending && !hosts.isError && <p className="notice notice-warning" role="status">默认电脑不在当前可访问列表中；本条消息不会自动发送到该电脑。</p>}
          <div className="assistant-target-row">
            <label><span>目标电脑</span><select aria-label="选择目标电脑" value={selectedHost?.id ?? hostSelection ?? ''}
              disabled={hosts.isPending || send.isPending || taskBusy}
              onChange={(event) => { setHostSelection(event.target.value); setWorkspaceSelection(''); }}>
              <option value="">无电脑目标</option>
              {hostSelection && !selectedHost && <option value={hostSelection}>所选电脑状态待核验</option>}
              {hostItems.map((host) => <option key={host.id} value={host.id}>{host.name || host.id} · {hostStateLabel[host.state] || host.state} · {controlStateLabels[host.control_state]}</option>)}
            </select></label>
            {selectedHost && <small className={hostIsReady ? 'muted' : 'assistant-target-unavailable'}>电脑状态：{hostStateLabel[selectedHost.state] || selectedHost.state} · {controlStateLabels[selectedHost.control_state]}</small>}
            {selectedHost && <label><span>Workspace</span><select aria-label="选择目标 Workspace" value={selectedBinding?.workspace.id ?? workspaceSelection}
              disabled={!hostIsReady || send.isPending || taskBusy} onChange={(event) => setWorkspaceSelection(event.target.value)}>
              <option value="">不附加 Workspace</option>
              {workspaceSelection && !selectedBinding && <option value={workspaceSelection}>所选 Workspace 状态待核验</option>}
              {workspaceBindings.map((binding) => <option key={binding.workspace.id} value={binding.workspace.id} disabled={!workspaceBindingReady(binding)}>
                {binding.workspace.name || binding.workspace.id} · v{binding.workspace.revision} · {workspaceBindingReady(binding) ? '已应用' : '未就绪'}
              </option>)}
            </select></label>}
            {workspaceSelection && !selectedBinding && <small className="assistant-target-unavailable" role="status">所选 Workspace 已不在当前电脑的可访问列表中，本条消息不会提交。</small>}
            {selectedBinding && <small className="muted">此消息固定使用已应用的 Workspace v{selectedBinding.workspace.revision}。</small>}
            <p className="assistant-target-hint">目标只对接下来发送的消息生效；已受理 Task 的电脑和 Workspace 版本不会随选择器改变。</p>
          </div>
          <form onSubmit={submit}>
            <label className="sr-only" htmlFor="assistant-content">消息内容</label>
            <textarea id="assistant-content" value={content} onChange={(event) => setContent(event.target.value)} maxLength={32768}
              placeholder="输入你想问的问题…" disabled={send.isPending || taskBusy || Boolean(conversationID && messages.isPending)} />
            <div className="assistant-composer-footer"><div className="assistant-selectors"><label><span>AI 模型</span><select aria-label="选择 AI 模型"
              value={effectiveModelID} onChange={(event) => setModelID(event.target.value)} disabled={models.isPending || send.isPending || taskBusy}>
              <option value="">选择模型</option>{readyModels.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.model_name}</option>)}
            </select></label></div>
              <button className="button button-primary assistant-send" type="submit" disabled={!content.trim() || !effectiveModelID || !taskAPIAvailable || send.isPending || taskBusy || targetUnavailable || Boolean(conversationID && messages.isPending)}>{send.isPending ? '正在提交…' : taskBusy ? '任务进行中' : '发送'} <span aria-hidden="true">↑</span></button>
            </div>
          </form>
          <p className="composer-hint">任务状态和回复会保存到当前对话；你可以在任务完成后切换模型继续。</p>
        </section>
      </div>
    </div>
  </section>;
}
