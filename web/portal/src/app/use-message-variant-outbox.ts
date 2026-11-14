import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { newIdempotencyKey } from './api';
import { messageVariantOutbox, messageVariantOutboxScope } from './message-variant-outbox';
import type { PendingMessageVariantOperation } from './message-variant-outbox';
import type { CreateMessageVariantInput } from './message-variants';

export type MessageVariantOutboxEntry = { scope: string; operation: PendingMessageVariantOperation };

export function useMessageVariantOutbox(actorID: string | undefined, serviceInstanceID: string | undefined) {
  const [entries, setEntries] = useState<MessageVariantOutboxEntry[]>([]);
  const [storageError, setStorageError] = useState<string | null>(null);
  const generation = useRef(0);
  const currentScope = actorID ? messageVariantOutboxScope(actorID, serviceInstanceID) : '';
  const unknownScope = actorID ? messageVariantOutboxScope(actorID, undefined) : '';
  const activeScope = useRef(currentScope);
  useLayoutEffect(() => { activeScope.current = currentScope; }, [currentScope]);

  const refresh = useCallback(async (recover = false) => {
    if (currentScope !== activeScope.current || !currentScope) return;
    const currentGeneration = ++generation.current;
    try {
      const scopes = currentScope === unknownScope ? [currentScope] : [currentScope, unknownScope];
      const lists = await Promise.all(scopes.map((scope) => recover
        ? messageVariantOutbox.update(scope, (items) => items.map((operation) => operation.status === 'sending'
          ? { ...operation, status: 'unknown', error: '请求结果待核对；重试会复用原操作编号。' } : operation))
        : messageVariantOutbox.read(scope)));
      if (currentGeneration !== generation.current) return;
      setEntries(scopes.flatMap((scope, index) => (lists[index] ?? []).map((operation) => ({ scope, operation })))
        .sort((left, right) => left.operation.createdAt - right.operation.createdAt));
      setStorageError(null);
    } catch (error) {
      if (currentGeneration === generation.current) setStorageError(error instanceof Error ? error.message : '无法读取本地消息版本操作。');
    }
  }, [currentScope, unknownScope]);

  useEffect(() => {
    // Mark a request that was interrupted by page shutdown as uncertain; never replay it automatically.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    void refresh(true);
    return () => { generation.current += 1; };
  }, [refresh]);

  const enqueue = useCallback(async (input: Omit<CreateMessageVariantInput, 'idempotencyKey'>) => {
    if (!currentScope) throw new Error('当前账号待确认，消息版本操作未发送。');
    const operation: PendingMessageVariantOperation = {
      input: { ...input, idempotencyKey: newIdempotencyKey() }, status: 'pending', createdAt: Date.now(),
    };
    await messageVariantOutbox.update(currentScope, (items) => {
      if (items.some((item) => item.input.idempotencyKey === operation.input.idempotencyKey)) return items;
      return [...items, operation];
    });
    await refresh();
    return { scope: currentScope, operation };
  }, [currentScope, refresh]);

  const change = useCallback(async (scope: string, key: string, update: (operation: PendingMessageVariantOperation) => PendingMessageVariantOperation) => {
    await messageVariantOutbox.update(scope, (items) => items.map((operation) => operation.input.idempotencyKey === key ? update(operation) : operation));
    await refresh();
  }, [refresh]);

  const remove = useCallback(async (scope: string, key: string) => {
    await messageVariantOutbox.update(scope, (items) => items.filter((operation) => operation.input.idempotencyKey !== key));
    await refresh();
  }, [refresh]);

  return { entries: entries.filter((entry) => entry.scope === currentScope || entry.scope === unknownScope),
    storageError, currentScope, unknownScope, enqueue, change, remove, refresh };
}
