import { useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { errorDetails, newIdempotencyKey, portalApi } from '../app/api';
import { formatDate } from '../app/format';
import { useToast } from '../app/toast';

type Conversation = Awaited<ReturnType<typeof portalApi.instantChatConversations>>['items'][number];

export function Assistant() {
  const client = useQueryClient();
  const { notify } = useToast();
  const [conversationID, setConversationID] = useState<string | null>(null);
  const [modelID, setModelID] = useState('');
  const [content, setContent] = useState('');
  const pendingSend = useRef<{ fingerprint: string; key: string } | null>(null);
  const conversations = useQuery({ queryKey: ['instant-chat-conversations'], queryFn: portalApi.instantChatConversations });
  const models = useQuery({ queryKey: ['model-connections'], queryFn: portalApi.modelConnections });
  const preferences = useQuery({ queryKey: ['user-preferences'], queryFn: portalApi.preferences });
  const selected = conversations.data?.items.find((item) => item.id === conversationID);
  const messages = useQuery({
    queryKey: ['instant-chat-messages', conversationID],
    queryFn: () => portalApi.instantChatMessages(conversationID!),
    enabled: Boolean(conversationID),
  });
  const readyModels = models.data?.items.filter((item) => item.checked_at && item.capabilities.text) ?? [];
  const chosenModelID = selected?.model_id || modelID || preferences.data?.selected_model_id || '';
  const effectiveModelID = readyModels.some((item) => item.id === chosenModelID) ? chosenModelID : '';
  const send = useMutation({
    mutationFn: ({ text, key }: { text: string; key: string }) => portalApi.sendInstantChatMessage({
      ...(conversationID ? { conversation_id: conversationID } : {}), model_id: effectiveModelID, content: text,
    }, key),
    onSuccess: async (result, request) => {
      if (pendingSend.current?.key === request.key) pendingSend.current = null;
      setContent('');
      setConversationID(result.conversation.id);
      await Promise.all([
        client.invalidateQueries({ queryKey: ['instant-chat-conversations'] }),
        client.invalidateQueries({ queryKey: ['instant-chat-messages', result.conversation.id] }),
      ]);
    },
    onError: (error) => notify(errorDetails(error).message, 'error'),
  });

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const text = content.trim();
    if (!text || !effectiveModelID || send.isPending) return;
    const fingerprint = JSON.stringify({ conversationID, modelID: effectiveModelID, text });
    if (pendingSend.current?.fingerprint !== fingerprint) pendingSend.current = { fingerprint, key: newIdempotencyKey() };
    send.mutate({ text, key: pendingSend.current.key });
  };

  const startNew = () => {
    pendingSend.current = null;
    setConversationID(null);
    setContent('');
  };

  return <section className="assistant-page">
    <div className="page-heading assistant-heading">
      <div><p className="eyebrow">Morde 云端电脑</p><h1>AI 助手</h1><p className="muted">直接与 AI 对话，回复会保存在这里。</p></div>
      <button type="button" className="button button-secondary" onClick={startNew} disabled={send.isPending}>＋ 新对话</button>
    </div>
    <div className="assistant-layout">
      <aside className="assistant-sidebar" aria-label="最近对话">
        <div className="assistant-sidebar-heading"><h2>最近对话</h2><button type="button" className="icon-button" aria-label="刷新对话" onClick={() => void conversations.refetch()}>↻</button></div>
        {conversations.isPending ? <p className="muted" role="status">正在加载对话…</p> : conversations.isError
          ? <div className="error-inline" role="alert">{errorDetails(conversations.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void conversations.refetch()}>重试</button></div>
          : !conversations.data.items.length ? <p className="muted assistant-sidebar-empty">还没有对话。发送一条消息即可开始。</p>
            : <div className="conversation-list">{conversations.data.items.map((item: Conversation) => <button type="button" key={item.id}
              className={`conversation-item ${conversationID === item.id ? 'is-selected' : ''}`} disabled={send.isPending}
              onClick={() => { setConversationID(item.id); setContent(''); pendingSend.current = null; }}>
              <strong>{item.title || '新对话'}</strong><small>{formatDate(item.updated_at)}</small>
            </button>)}</div>}
      </aside>
      <div className="assistant-main">
        {!conversationID ? <div className="assistant-welcome"><span className="assistant-mark" aria-hidden="true">M</span>
          <h2>今天想聊些什么？</h2><p>直接发送问题，AI 会在这里回复。当前对话不会操作你的云端电脑或文件。</p>
        </div> : <div className="conversation-view" aria-live="polite">
          <header className="conversation-view-heading"><div><p className="eyebrow">即时对话</p><h2>{selected?.title || '对话'}</h2></div></header>
          {messages.isPending ? <div className="loading-card" role="status">正在加载消息…</div>
            : messages.isError ? <div className="error-card" role="alert">{errorDetails(messages.error).message}<button type="button" className="button button-secondary" onClick={() => void messages.refetch()}>重试</button></div>
              : <div className="message-list">{messages.data.items.map((message) => <article className={`chat-message chat-message-${message.role}`} key={message.id}>
                <strong className="chat-message-author">{message.role === 'user' ? '你' : 'AI 助手'}</strong><p>{message.content}</p><small>{formatDate(message.created_at)}</small>
              </article>)}</div>}
        </div>}
        <section className="assistant-composer" aria-label="发送即时对话消息">
          {models.isError && <div className="error-inline" role="alert">无法读取模型：{errorDetails(models.error).message}<button type="button" className="button button-small button-secondary" onClick={() => void models.refetch()}>重试</button></div>}
          {!models.isPending && !models.isError && !readyModels.length && <div className="notice notice-warning assistant-setup-notice" role="status">还没有可用的 AI 模型。<Link to="/connections">检查模型设置</Link></div>}
          <form onSubmit={submit}>
            <label className="sr-only" htmlFor="assistant-content">消息内容</label>
            <textarea id="assistant-content" value={content} onChange={(event) => setContent(event.target.value)} maxLength={32768}
              placeholder="输入你想问的问题…" disabled={send.isPending} />
            <div className="assistant-composer-footer"><div className="assistant-selectors"><label><span>AI 模型</span><select aria-label="选择 AI 模型"
              value={effectiveModelID} onChange={(event) => setModelID(event.target.value)} disabled={Boolean(conversationID) || models.isPending || send.isPending}>
              <option value="">选择模型</option>{readyModels.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.model_name}</option>)}
            </select></label></div>
              <button className="button button-primary assistant-send" type="submit" disabled={!content.trim() || !effectiveModelID || send.isPending || (Boolean(conversationID) && messages.isPending)}>{send.isPending ? '正在回复…' : '发送'} <span aria-hidden="true">↑</span></button>
            </div>
          </form>
          <p className="composer-hint">消息和 AI 回复会保存在当前对话中。</p>
        </section>
      </div>
    </div>
  </section>;
}
