import { cleanup, renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { messageVariantOutbox, messageVariantOutboxScope } from './message-variant-outbox';
import type { PendingMessageVariantOperation } from './message-variant-outbox';
import * as messageVariantAPI from './message-variants';
import { useMessageVariantOutbox } from './use-message-variant-outbox';

const actorID = 'u1234567';
const serviceInstanceID = 'instance-a';
const scope = messageVariantOutboxScope(actorID, serviceInstanceID);
const input = {
  conversationID: 'c1234567', messageID: 'm1234567', action: 'edit_resend' as const, expectedVersion: 2,
  content: '保持原操作内容', serviceInstanceID, actorID,
};

describe('message variant outbox hook', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('marks an interrupted request uncertain without replaying it', async () => {
    const pending: PendingMessageVariantOperation = {
      input: { ...input, idempotencyKey: 'variant-replay-01' }, status: 'sending', createdAt: 1,
    };
    const values = new Map<string, PendingMessageVariantOperation[]>([[scope, [pending]]]);
    vi.spyOn(messageVariantOutbox, 'read').mockImplementation(async (key) => values.get(key) ?? []);
    vi.spyOn(messageVariantOutbox, 'update').mockImplementation(async (key, change) => {
      const next = change(values.get(key) ?? []);
      values.set(key, next);
      return next;
    });
    const replay = vi.spyOn(messageVariantAPI, 'createMessageVariant');

    const { result } = renderHook(() => useMessageVariantOutbox(actorID, serviceInstanceID));

    await waitFor(() => expect(result.current.entries[0]?.operation.status).toBe('unknown'));
    expect(result.current.entries[0]?.operation.input.idempotencyKey).toBe('variant-replay-01');
    expect(replay).not.toHaveBeenCalled();
  });

  it('retains the same request key and body for an explicit retry', async () => {
    const values = new Map<string, PendingMessageVariantOperation[]>();
    vi.spyOn(messageVariantOutbox, 'read').mockImplementation(async (key) => values.get(key) ?? []);
    vi.spyOn(messageVariantOutbox, 'update').mockImplementation(async (key, change) => {
      const next = change(values.get(key) ?? []);
      values.set(key, next);
      return next;
    });
    const { result } = renderHook(() => useMessageVariantOutbox(actorID, serviceInstanceID));
    await waitFor(() => expect(result.current.storageError).toBeNull());

    const added = await result.current.enqueue(input);
    const originalKey = added.operation.input.idempotencyKey;
    await result.current.change(added.scope, originalKey, (operation) => ({ ...operation, status: 'unknown', error: '网络中断' }));
    const persisted = await messageVariantOutbox.read(scope);

    expect(persisted).toEqual([expect.objectContaining({
      input: { ...input, idempotencyKey: originalKey }, status: 'unknown', error: '网络中断',
    })]);
  });
});
