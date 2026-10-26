import { useEffect, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link, useSearchParams } from 'react-router';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import type { components } from '../api/schema';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';
import { isActiveTaskStatus, taskStatusLabel } from './Tasks';

type Conversation = Awaited<ReturnType<typeof portalApi.conversations>>['items'][number];
type Message = Awaited<ReturnType<typeof portalApi.conversationMessages>>['items'][number];
type LiveAssistantResponse = { messageID: string; turnID: string; content: string; done: boolean };
type SendRequest = { content: string; hostID: string; modelID: string; conversationID: string | null; idempotencyKey: string };

const eventTypes = ['task.status', 'task.step', 'task.operation', 'task.message', 'task.batch_item', 'task.artifact', 'task.waiting_user', 'task.response', 'task.result', 'task.snapshot_required'] as const;

export function Assistant() {
  const client = useQueryClient();
  const { notify } = useToast();
  const [searchParams] = useSearchParams();
  const requestedHostID = searchParams.get('host_id') ?? '';
  const [conversationID, setConversationID] = useState<string | null>(null);
  const [hostID, setHostID] = useState(requestedHostID);
  const [modelID, setModelID] = useState('');
  const [objective, setObjective] = useState('');
  const pendingSend = useRef<{ fingerprint: string; idempotencyKey: string } | null>(null);
  const hosts = useQuery({ queryKey: ['hosts'], queryFn: portalApi.hosts, refetchInterval: (query) => query.state.data?.items.some((host) => host.state === 'creating' || host.state === 'stopping') ? 3000 : false });
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections });
  const preferences = useQuery({ queryKey: ['user-preferences'], queryFn: portalApi.preferences });
  const onboarding = useQuery({
    queryKey: ['onboarding-status'], queryFn: portalApi.onboardingStatus,
    refetchInterval: (query) => ['creating', 'restoring', 'stopping'].includes(query.state.data?.computer_status ?? '') ? 3000 : false,
  });
  const conversations = useQuery({ queryKey: ['conversations'], queryFn: () => portalApi.conversations() });
  const selectedConversation = conversations.data?.items.find((item) => item.id === conversationID);
  const messages = useInfiniteQuery({
    queryKey: ['conversation-messages', conversationID],
    queryFn: ({ pageParam }) => portalApi.conversationMessages(conversationID!, pageParam),
    initialPageParam: undefined as string | undefined,
    getNextPageParam: (page) => page.next_cursor ?? undefined,
    enabled: Boolean(conversationID),
  });
  const messageItems = messages.data?.pages.slice().reverse().flatMap((page) => page.items) ?? [];
  const latestTaskID = conversationID ? messageItems.at(-1)?.task_id : undefined;
  const latestTask = useQuery({
    queryKey: ['task', latestTaskID], queryFn: () => portalApi.task(latestTaskID!), enabled: Boolean(latestTaskID),
    refetchInterval: (query) => query.state.data && isActiveTaskStatus(query.state.data.status) ? 5000 : false,
  });
  const conversationTargetReady = !conversationID || (!messages.isPending && (!latestTaskID || latestTask.isSuccess));
  const activeHosts = hosts.data?.items.filter((host) => host.state === 'running') ?? [];
  const readyModelIDs = new Set(onboarding.data?.models.map((model) => model.id) ?? []);
  const usableModels = models.data?.items.filter((model) => readyModelIDs.has(model.id)) ?? [];
  const defaultHostID = onboarding.data?.default_host_id ?? preferences.data?.default_host_id ?? '';

  const effectiveHostID = (selectedConversation?.host_id ?? hostID) || defaultHostID || (activeHosts.length === 1 ? activeHosts[0]!.id : '');
  const effectiveModelID = modelID || preferences.data?.selected_model_id || onboarding.data?.selected_model_id || '';

  const savePreferences = useMutation({
    mutationFn: (selectedModelID: string) => portalApi.savePreferences({
      assistant_name: preferences.data?.assistant_name ?? 'Morde', selected_model_id: selectedModelID,
    }),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['user-preferences'] }),
        client.invalidateQueries({ queryKey: ['onboarding-status'] }),
      ]);
      notify('默认模型已保存。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });
  const setDefaultHost = useMutation({
    mutationFn: (id: string | undefined) => portalApi.ensureDefaultHost(id ? { host_id: id } : {}),
    onSuccess: async (result) => {
      setHostID(result.status.default_host_id ?? result.host?.id ?? '');
      await Promise.all([
        client.invalidateQueries({ queryKey: ['hosts'] }),
        client.invalidateQueries({ queryKey: ['user-preferences'] }),
        client.invalidateQueries({ queryKey: ['onboarding-status'] }),
      ]);
      notify(result.created ? '已创建云端电脑，正在准备运行环境。' : result.status.computer_status === 'creating' || result.status.computer_status === 'restoring' ? '正在准备云端电脑。' : '默认云端电脑已更新。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const send = useMutation({
    mutationFn: async ({ content, hostID, modelID, conversationID, idempotencyKey }: SendRequest) => {
      const body = { host_id: hostID, model_id: modelID, objective: content } satisfies components['schemas']['SubmitTaskRequest'];
      if (!conversationID) return portalApi.submitTask(body, idempotencyKey);
      const appended = await portalApi.appendConversationMessage(conversationID, {
        host_id: hostID, model_id: modelID, content,
      }, idempotencyKey);
      return appended.task;
    },
    onSuccess: async (task, request) => {
      if (pendingSend.current?.idempotencyKey === request.idempotencyKey) pendingSend.current = null;
      setObjective('');
      setConversationID(task.conversation_id);
      await Promise.all([
        client.invalidateQueries({ queryKey: ['conversations'] }),
        client.invalidateQueries({ queryKey: ['conversation-messages', task.conversation_id] }),
        client.invalidateQueries({ queryKey: ['tasks'] }),
      ]);
      notify('任务已接收，可在此查看进度。', 'success');
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const content = objective.trim();
    if (!content || !effectiveHostID || !effectiveModelID || !conversationTargetReady || send.isPending) return;
    const request = { content, hostID: effectiveHostID, modelID: effectiveModelID, conversationID };
    const fingerprint = JSON.stringify(request);
    if (pendingSend.current?.fingerprint !== fingerprint) {
      pendingSend.current = { fingerprint, idempotencyKey: newIdempotencyKey() };
    }
    send.mutate({ ...request, idempotencyKey: pendingSend.current.idempotencyKey });
  };
  const startNew = () => { pendingSend.current = null; setConversationID(null); setObjective(''); };
  const selectedHost = hosts.data?.items.find((host) => host.id === effectiveHostID);
  const modelReady = onboarding.data?.model_status === 'ready';
  const computerReady = selectedConversation ? selectedHost?.state === 'running' : onboarding.data?.can_submit === true;
  const canSend = Boolean(computerReady && modelReady && selectedHost?.state === 'running' && effectiveModelID && objective.trim() && conversationTargetReady && !send.isPending && !savePreferences.isPending);

  return <section className="assistant-page">
    <div className="page-heading assistant-heading">
      <div><p className="eyebrow">Morde 云端电脑</p><h1>AI 助手</h1><p className="muted">把任务交给云端电脑，离开页面后任务仍会继续。你可以随时回来查看结果。</p></div>
      <button type="button" className="button button-secondary" onClick={startNew} disabled={send.isPending}>＋ 新任务</button>
    </div>
    <div className="assistant-layout">
      <aside className="assistant-sidebar" aria-label="最近对话">
        <div className="assistant-sidebar-heading"><h2>最近任务</h2><button type="button" className="icon-button" aria-label="刷新对话" title="刷新对话" onClick={() => void conversations.refetch()}>↻</button></div>
        {conversations.isPending ? <p className="muted" role="status">正在加载对话…</p>
          : conversations.isError ? <div className="error-inline" role="alert">{errorDetails(conversations.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void conversations.refetch()}>重试</button></div>
            : !conversations.data.items.length ? <p className="muted assistant-sidebar-empty">还没有任务。描述你要做的事即可开始。</p>
              : <div className="conversation-list">{conversations.data.items.map((item) => <button type="button" key={item.id} className={`conversation-item ${conversationID === item.id ? 'is-selected' : ''}`} onClick={() => { setConversationID(item.id); }} disabled={send.isPending}>
                <strong>{item.title || '新任务'}</strong><small>{formatDate(item.updated_at)}</small>
              </button>)}</div>}
      </aside>
      <div className="assistant-main">
        {!conversationID ? <div className="assistant-welcome">
          <span className="assistant-mark" aria-hidden="true">M</span>
          <h2>今天想让云端电脑做什么？</h2>
          <p>写清目标和希望得到的结果。任务会保存在云端，电脑环境与文件也会保留。</p>
          <div className="assistant-suggestions" aria-label="任务示例">
            {['整理工作目录里的项目文件，并生成一份摘要', '打开浏览器查看指定网页并整理关键信息', '检查当前项目，列出最值得优先处理的问题'].map((example) => <button type="button" key={example} onClick={() => setObjective(example)}>{example}<span aria-hidden="true">↗</span></button>)}
          </div>
        </div> : <ConversationView conversation={selectedConversation} messages={messageItems} loading={messages.isPending}
          error={messages.isError && !messages.data ? errorDetails(messages.error).message : null} onRetry={() => void messages.refetch()}
          hasEarlierMessages={messages.hasNextPage} loadingEarlierMessages={messages.isFetchingNextPage}
          loadEarlierError={messages.isFetchNextPageError ? errorDetails(messages.error).message : null}
          onLoadEarlierMessages={() => void messages.fetchNextPage()} />}
        <section className="assistant-composer" aria-label="提交云端任务">
          {hosts.isError && <div className="error-inline" role="alert">无法读取云端电脑：{errorDetails(hosts.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void hosts.refetch()}>重试</button></div>}
          {models.isError && <div className="error-inline" role="alert">无法读取模型：{errorDetails(models.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void models.refetch()}>重试</button></div>}
          {onboarding.isError && <div className="error-inline" role="alert">无法读取首次使用状态：{errorDetails(onboarding.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void onboarding.refetch()}>重试</button></div>}
          {conversationID && latestTask.isError && <div className="error-inline" role="alert">无法读取当前任务目标：{errorDetails(latestTask.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void latestTask.refetch()}>重试</button></div>}
          {!onboarding.isPending && onboarding.data && !onboarding.data.can_submit && <div className="notice notice-warning assistant-setup-notice" role="status">
            <strong>{setupTitle(onboarding.data.model_status, onboarding.data.computer_status)}</strong>
            <span>{setupDescription(onboarding.data.model_status, onboarding.data.computer_status)}</span>
            <div className="assistant-setup-actions">
              {onboarding.data.computer_status === 'not_created' && <button type="button" className="button button-small button-primary" disabled={setDefaultHost.isPending} onClick={() => setDefaultHost.mutate(undefined)}>{setDefaultHost.isPending ? '正在创建…' : '创建我的云端电脑'}</button>}
              {onboarding.data.computer_status === 'stopped' && <button type="button" className="button button-small button-primary" disabled={setDefaultHost.isPending} onClick={() => setDefaultHost.mutate(undefined)}>{setDefaultHost.isPending ? '正在恢复…' : '恢复云端电脑'}</button>}
              {(onboarding.data.computer_status === 'available' || onboarding.data.computer_status === 'selection_required') && effectiveHostID && effectiveHostID !== defaultHostID && <button type="button" className="button button-small button-primary" disabled={setDefaultHost.isPending} onClick={() => setDefaultHost.mutate(effectiveHostID)}>{setDefaultHost.isPending ? '正在保存…' : '设为默认电脑'}</button>}
              {onboarding.data.computer_status === 'creating' || onboarding.data.computer_status === 'restoring' || onboarding.data.computer_status === 'stopping' ? <span role="status">状态更新中…</span> : null}
              {(onboarding.data.model_status === 'not_configured' || onboarding.data.model_status === 'selected_unavailable') && <Link className="button button-small button-secondary" to="/connections">检查模型设置</Link>}
            </div>
          </div>}
          <form onSubmit={(event) => void submit(event)}>
            <label className="sr-only" htmlFor="assistant-objective">要完成的任务</label>
            <textarea id="assistant-objective" value={objective} onChange={(event) => setObjective(event.target.value)} maxLength={32768} placeholder="描述要完成的任务、文件或网站，以及你希望得到的结果…" disabled={send.isPending} />
            <div className="assistant-composer-footer">
              <div className="assistant-selectors">
                <label><span>云端电脑</span><select aria-label="选择云端电脑" value={effectiveHostID} onChange={(event) => { setHostID(event.target.value); }} disabled={Boolean(conversationID) || hosts.isPending || !hosts.data?.items.length}>
                  <option value="">{hosts.isPending ? '读取中…' : '选择电脑'}</option>{(hosts.data?.items ?? []).map((host) => <option key={host.id} value={host.id}>{host.name || '未命名电脑'} · {host.state === 'running' ? '运行中' : host.state === 'stopped' ? '已停止' : host.state}</option>)}
                </select></label>
                <label><span>AI 模型</span><select aria-label="选择 AI 模型" value={effectiveModelID} onChange={(event) => { const value = event.target.value; setModelID(value); savePreferences.mutate(value); }} disabled={models.isPending || usableModels.length === 0 || savePreferences.isPending}>
                  <option value="">{models.isPending ? '读取中…' : '选择模型'}</option>{usableModels.map((model) => <option key={model.id} value={model.id}>{model.name} · {model.model_name}</option>)}
                </select></label>
                {conversationID && <span className="muted assistant-host-lock">对话固定使用 {selectedConversation?.host_id ?? '当前电脑'}</span>}
              </div>
              <button className="button button-primary assistant-send" type="submit" disabled={!canSend}>{send.isPending ? '正在提交…' : '交办任务'} <span aria-hidden="true">↑</span></button>
            </div>
          </form>
          <p className="composer-hint">任务执行和用量受平台策略限制。发出任务后可关闭网页，稍后从“任务”中查看进展。</p>
        </section>
      </div>
    </div>
  </section>;
}

function setupTitle(model: components['schemas']['OnboardingStatus']['model_status'], computer: components['schemas']['OnboardingStatus']['computer_status']): string {
  if (model === 'not_configured') return '还没有可用的 AI 模型';
  if (model === 'selection_required') return '选择一个 AI 模型';
  if (model === 'selected_unavailable') return '当前默认模型暂不可用';
  if (computer === 'not_created') return '创建你的云端电脑';
  if (computer === 'available') return '选择要使用的云端电脑';
  if (computer === 'selection_required') return '选择默认云端电脑';
  if (computer === 'creating' || computer === 'restoring' || computer === 'stopping') return '正在准备云端电脑';
  if (computer === 'stopped') return '云端电脑已停止';
  if (computer === 'capacity_unavailable') return '当前容量不足';
  if (computer === 'quota_exceeded') return '已达到电脑配额';
  if (computer === 'create_failed' || computer === 'restore_failed' || computer === 'unavailable') return '云端电脑暂不可用';
  return '完成首次使用设置';
}

function setupDescription(model: components['schemas']['OnboardingStatus']['model_status'], computer: components['schemas']['OnboardingStatus']['computer_status']): string {
  if (model === 'not_configured') return '请联系管理员配置模型，或在连接设置中添加个人模型。';
  if (model === 'selection_required') return '选定模型后，Morde 会记住你的选择。';
  if (model === 'selected_unavailable') return '请检查连接设置，或选择其他可用模型。';
  if (computer === 'not_created') return '创建后，AI 任务和文件会保存在这台独立的云端电脑中。';
  if (computer === 'available' || computer === 'selection_required') return '选择一台运行中的电脑并设为默认，后续任务会继续使用它。';
  if (computer === 'creating' || computer === 'restoring' || computer === 'stopping') return '状态会自动刷新；准备完成后即可交办任务。';
  if (computer === 'stopped') return '恢复后会保留工作目录和文件。';
  if (computer === 'capacity_unavailable') return '稍后可以重试；现有文件不会受影响。';
  if (computer === 'quota_exceeded') return '请联系管理员调整你的电脑配额。';
  if (computer === 'create_failed' || computer === 'restore_failed' || computer === 'unavailable') return '可以选择另一台电脑，或前往电脑页面查看状态。';
  return '完成模型和电脑选择后即可开始。';
}

function ConversationView({ conversation, messages, loading, error, onRetry, hasEarlierMessages, loadingEarlierMessages, loadEarlierError, onLoadEarlierMessages }: {
  conversation?: Conversation;
  messages: Message[];
  loading: boolean;
  error: string | null;
  onRetry: () => void;
  hasEarlierMessages: boolean;
  loadingEarlierMessages: boolean;
  loadEarlierError: string | null;
  onLoadEarlierMessages: () => void;
}) {
  const latestUserMessageIndexByTask = new Map<string, number>();
  messages.forEach((message, index) => {
    if (message.role === 'user') latestUserMessageIndexByTask.set(message.task_id, index);
  });

  return <div className="conversation-view" aria-live="polite">
    <header className="conversation-view-heading"><div><p className="eyebrow">云端会话</p><h2>{conversation?.title || '任务对话'}</h2></div>{conversation && <span className="muted">电脑：{conversation.host_id}</span>}</header>
    {loading ? <div className="loading-card" role="status">正在恢复对话…</div> : error ? <div className="error-card" role="alert">{error}<button type="button" className="button button-secondary" onClick={onRetry}>重试</button></div>
      : !messages.length ? <div className="empty-card compact"><p>这个会话还没有消息。你可以在下方继续交办任务。</p></div>
        : <div className="message-list">
          {hasEarlierMessages && <div className="message-history-controls"><button type="button" className="button button-secondary button-small" disabled={loadingEarlierMessages} onClick={onLoadEarlierMessages}>{loadingEarlierMessages ? '正在加载…' : '加载更早消息'}</button>
            {loadEarlierError && <p className="error-inline" role="alert">{loadEarlierError}</p>}</div>}
          {messages.map((message, index) => <article className="message-block" key={message.id}>
          <div className={`chat-message chat-message-${message.role}`}><span className="chat-message-author">{message.role === 'user' ? '你' : 'Morde'}</span><p>{message.content}</p><small>{formatDate(message.created_at)}</small></div>
          {message.role === 'user' && latestUserMessageIndexByTask.get(message.task_id) === index && <TaskActivity key={message.task_id} taskID={message.task_id} conversationID={conversation?.id ?? ''}
            persistedAssistantMessageIDs={messages.filter((item) => item.task_id === message.task_id && item.role === 'assistant').map((item) => item.id)} />}
        </article>)}</div>}
  </div>;
}

function TaskActivity({ taskID, conversationID, persistedAssistantMessageIDs }: { taskID: string; conversationID: string; persistedAssistantMessageIDs: string[] }) {
  const client = useQueryClient();
  const { notify } = useToast();
  const [liveResponse, setLiveResponse] = useState<LiveAssistantResponse | null>(null);
  const taskQuery = useQuery({
    queryKey: ['task', taskID], queryFn: () => portalApi.task(taskID),
    refetchInterval: (query) => query.state.data && isActiveTaskStatus(query.state.data.status) ? 5000 : false,
  });
  const task = taskQuery.data;
  const shouldStreamTaskEvents = Boolean(task && isActiveTaskStatus(task.status));
  useEffect(() => {
    if (!shouldStreamTaskEvents || typeof EventSource === 'undefined') return;
    let snapshotRecoveryPending = false;
    const events = new EventSource(`/api/tasks/${encodeURIComponent(taskID)}/events`);
    const refresh = () => { void client.invalidateQueries({ queryKey: ['task', taskID] }); };
    const recoverSnapshot = () => {
      snapshotRecoveryPending = true;
      setLiveResponse(null);
      notify('任务事件不完整，已重新读取保存的状态；完整回复保存后会显示。', 'info');
      refresh();
      if (conversationID) void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
    };
    const receiveMessage = (event: Event) => {
      const message = parseAssistantMessageEvent((event as MessageEvent<string>).data);
      if (!message) return;
      if (snapshotRecoveryPending) {
        if (message.done) {
          snapshotRecoveryPending = false;
          refresh();
          if (conversationID) void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
        }
        return;
      }
      if (message.done) {
        setLiveResponse(message);
        refresh();
        if (conversationID) {
          void client.invalidateQueries({ queryKey: ['conversation-messages', conversationID] });
        }
        return;
      }
      setLiveResponse((current) => {
        if (current?.messageID === message.messageID && current.turnID === message.turnID && current.done) return current;
        const content = current?.messageID === message.messageID && current.turnID === message.turnID ? current.content : '';
        return { ...message, content: content + message.content, done: false };
      });
    };
    const listenerFor = (eventType: typeof eventTypes[number]): EventListener => eventType === 'task.message' ? receiveMessage : eventType === 'task.snapshot_required' ? recoverSnapshot : refresh;
    for (const eventType of eventTypes) events.addEventListener(eventType, listenerFor(eventType));
    events.onerror = refresh;
    return () => {
      for (const eventType of eventTypes) events.removeEventListener(eventType, listenerFor(eventType));
      events.close();
    };
  }, [client, conversationID, notify, shouldStreamTaskEvents, taskID]);

  if (taskQuery.isPending) return <div className="task-activity" role="status">正在读取任务进度…</div>;
  if (taskQuery.isError || !task) return <div className="task-activity task-activity-error" role="alert">任务状态暂时无法读取。<button className="button button-small button-secondary" type="button" onClick={() => void taskQuery.refetch()}>重试</button></div>;
  return <section className="task-activity">
    {liveResponse && !persistedAssistantMessageIDs.includes(liveResponse.messageID) && <div className="chat-message chat-message-assistant task-activity-response" role="status">
      <span className="chat-message-author">Morde · {liveResponse.done ? '回复已完成' : task.status === 'failed' ? '回复中断' : '正在回复'}</span>
      <p>{liveResponse.content || '正在组织回复…'}</p>
    </div>}
    <div className="task-activity-heading"><strong>云端任务</strong><span className={`status status-${task.status}`}><span aria-hidden="true" className="status-dot" />{taskStatusLabel[task.status]}</span></div>
    {task.current_step && <p className="task-activity-current">当前步骤：{task.current_step}</p>}
    {task.steps.length > 0 && <ol className="task-activity-steps">{task.steps.slice(-3).map((step) => <li key={step.id}><span className={`task-step-marker task-step-${step.status}`} aria-hidden="true" /><span><strong>{step.title}</strong>{step.summary && <small>{step.summary}</small>}</span></li>)}</ol>}
    {task.result && <div className="task-activity-result"><strong>{task.status === 'completed' ? '执行结果' : task.status === 'failed' ? '失败说明' : '最新情况'}</strong><p>{task.result}</p></div>}
    {task.status === 'waiting_user' && <p className="notice notice-warning">任务需要你的处理。<Link to={`/tasks/${encodeURIComponent(task.id)}`}>查看并处理</Link></p>}
    {task.artifacts.length > 0 && <p className="task-activity-artifact">已生成 {task.artifacts.length} 个文件产物。<Link to={`/tasks/${encodeURIComponent(task.id)}`}>查看路径</Link></p>}
    <Link className="task-activity-link" to={`/tasks/${encodeURIComponent(task.id)}`}>查看完整进度与用量 →</Link>
  </section>;
}

function parseAssistantMessageEvent(data: string): LiveAssistantResponse | null {
  try {
    const event = JSON.parse(data) as { payload?: { role?: unknown; message_id?: unknown; turn_id?: unknown; delta?: unknown; content?: unknown; done?: unknown } };
    const payload = event.payload;
    if (payload?.role !== 'assistant' || typeof payload.message_id !== 'string' || !payload.message_id || typeof payload.turn_id !== 'string' || !payload.turn_id) return null;
    if (payload.done === true && typeof payload.content === 'string') {
      return { messageID: payload.message_id, turnID: payload.turn_id, content: payload.content, done: true };
    }
    if (payload.done !== true && typeof payload.delta === 'string') {
      return { messageID: payload.message_id, turnID: payload.turn_id, content: payload.delta, done: false };
    }
    return null;
  } catch {
    return null;
  }
}
