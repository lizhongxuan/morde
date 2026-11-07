import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { bindOutboxConversation, enqueueOutboxMessage, messageOutbox, messageOutboxScope } from './message-outbox';
import type { PendingConversationMessage } from './message-outbox';

export type MessageOutboxEntry = { scope: string; message: PendingConversationMessage };

export function useMessageOutbox(userID: string | undefined, serviceInstanceID: string | undefined) {
  const [entries, setEntries] = useState<MessageOutboxEntry[]>([]);
  const [storageError, setStorageError] = useState<string | null>(null);
  const generation = useRef(0);
  const currentScope = userID ? messageOutboxScope(userID, serviceInstanceID) : '';
  const unknownScope = userID ? messageOutboxScope(userID, undefined) : '';
  const activeScope = useRef(currentScope);
  useLayoutEffect(() => { activeScope.current = currentScope; }, [currentScope]);
  const refresh = useCallback(async (recover = false) => {
    if (currentScope !== activeScope.current) return;
    const currentGeneration = ++generation.current;
    if (!currentScope) return;
    try {
      const scopes = currentScope === unknownScope ? [currentScope] : [currentScope, unknownScope];
      const lists = await Promise.all(scopes.map((scope) => recover
        ? messageOutbox.update(scope, (items) => items.map((message) => message.status === 'sending' ? { ...message, status: 'unknown' } : message))
        : messageOutbox.read(scope)));
      if (currentGeneration !== generation.current) return;
      const seen = new Set<string>();
      const next: MessageOutboxEntry[] = [];
      for (const [index, scope] of scopes.entries()) {
        for (const message of lists[index] ?? []) {
          if (seen.has(message.request.client_message_id)) continue;
          seen.add(message.request.client_message_id);
          next.push({ scope, message });
        }
      }
      setEntries(next.sort((a, b) => a.message.createdAt - b.message.createdAt));
      setStorageError(null);
    } catch (error) {
      if (currentGeneration === generation.current) setStorageError(error instanceof Error ? error.message : '无法读取本地消息。');
    }
  }, [currentScope, unknownScope]);

  useEffect(() => {
    // Refresh updates state only after IndexedDB completes its asynchronous read.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void refresh(true);
    return () => { generation.current += 1; };
  }, [refresh]);

  const add = useCallback(async (message: PendingConversationMessage) => {
    if (!currentScope) throw new Error('当前账号待确认，输入已保留。');
    await messageOutbox.update(currentScope, (items) => items.some((item) => item.request.client_message_id === message.request.client_message_id)
      ? items.map((item) => item.request.client_message_id === message.request.client_message_id ? message : item)
      : [...items, message]);
    await refresh();
    return currentScope;
  }, [currentScope, refresh]);

  const enqueue = useCallback(async (message: PendingConversationMessage, serverPredecessor?: string) => {
    if (!currentScope) throw new Error('当前账号待确认，输入已保留。');
    const saved = await enqueueOutboxMessage(messageOutbox, currentScope, message, serverPredecessor);
    await refresh();
    return { scope: currentScope, message: saved };
  }, [currentScope, refresh]);

  const bindConversation = useCallback(async (scope: string, actorID: string, createKey: string, conversationID: string) => {
    await bindOutboxConversation(messageOutbox, scope, actorID, createKey, conversationID);
    await refresh();
  }, [refresh]);

  const change = useCallback(async (scope: string, id: string, update: (message: PendingConversationMessage) => PendingConversationMessage) => {
    await messageOutbox.update(scope, (items) => items.map((item) => item.request.client_message_id === id ? update(item) : item));
    await refresh();
  }, [refresh]);

  const remove = useCallback(async (scope: string, id: string) => {
    await messageOutbox.update(scope, (items) => items.filter((item) => item.request.client_message_id !== id));
    await refresh();
  }, [refresh]);

  return { entries: currentScope ? entries.filter((entry) => entry.scope === currentScope || entry.scope === unknownScope) : [],
    storageError, currentScope, unknownScope, add, enqueue, bindConversation, change, remove, refresh };
}
