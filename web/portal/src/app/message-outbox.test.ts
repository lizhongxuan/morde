import { describe, expect, it } from 'vitest';
import { bindOutboxConversation, enqueueOutboxMessage, outboxReceiptRequest } from './message-outbox';
import type { MessageOutboxStorage, OutboxOrdering, PendingConversationMessage } from './message-outbox';

const attachment = {
  id: 'attachment-001', name: 'synthetic.txt', mediaType: 'text/plain', blob: new Blob(['synthetic']),
  objectID: 'object-001', objectKey: 'object-key-001', uploadKey: 'upload-key-001', commitKey: 'commit-key-001',
  attachmentID: 'pa_0123456789abcdef0123456789abcdef',
};

function entry(request: unknown, attachments = [attachment]): PendingConversationMessage {
  return { request: request as PendingConversationMessage['request'], actorID: 'u1234567', serviceInstanceID: 'instance-001',
    conversationID: 'c1234567', attachments, status: 'pending', createdAt: 1 };
}

describe('outbox receipt request', () => {
  it('keeps the accepted message, target and attachment slot', () => {
    const original = entry({ client_message_id: 'message-001', content: '完整原文', reference_task_id: 'q1234567',
      model_id: 'p1234567', workspace_id: 'w1234567', client_attachment_ids: [attachment.id] });
    expect(outboxReceiptRequest(original)).toEqual({ client_message_id: 'message-001', content: '完整原文',
      reference_task_id: 'q1234567', model_id: 'p1234567', workspace_id: 'w1234567', client_attachment_ids: [attachment.id] });
    expect(original.attachments[0]?.blob).toBe(attachment.blob);
  });

  it('keeps incomplete attachment submissions from losing their slots', () => {
    expect(() => outboxReceiptRequest(entry({ client_message_id: 'message-001', content: '原文', client_attachment_ids: ['other-slot'] })))
      .toThrow('消息附件资料不完整');
    expect(() => outboxReceiptRequest(entry({ client_message_id: 'message-002', content: '原文', client_attachment_ids: ['attachment-001'] }, [])))
      .toThrow('消息附件资料不完整');
  });
});

class MemoryOutbox implements MessageOutboxStorage {
  private items = new Map<string, PendingConversationMessage[]>();
  private order = new Map<string, OutboxOrdering>();
  private next: Promise<void> = Promise.resolve();

  async read(scope: string) { return this.items.get(scope) ?? []; }

  update(scope: string, change: (items: PendingConversationMessage[], ordering: OutboxOrdering) => PendingConversationMessage[]) {
    const operation = this.next.then(() => {
      const prior = this.order.get(scope) ?? { heads: {}, aliases: {} };
      const ordering = { heads: { ...prior.heads }, aliases: { ...prior.aliases } };
      const result = change(this.items.get(scope) ?? [], ordering);
      this.items.set(scope, result);
      this.order.set(scope, ordering);
      return result;
    });
    this.next = operation.then(() => undefined, () => undefined);
    return operation;
  }
}

describe('outbox message ordering', () => {
  it('serializes simultaneous messages and keeps the head after their local entries are removed', async () => {
    const storage = new MemoryOutbox();
    const first = entry({ client_message_id: 'message-001', content: '一' }, []);
    const second = entry({ client_message_id: 'message-002', content: '二' }, []);
    const [savedFirst, savedSecond] = await Promise.all([
      enqueueOutboxMessage(storage, 'account-service', first, 'server-previous'),
      enqueueOutboxMessage(storage, 'account-service', second, 'server-previous'),
    ]);
    expect(savedFirst.request.previous_client_message_id).toBe('server-previous');
    expect(savedSecond.request.previous_client_message_id).toBe('message-001');
    await storage.update('account-service', () => []);
    const third = await enqueueOutboxMessage(storage, 'account-service', entry({ client_message_id: 'message-003', content: '三' }, []), 'stale-server');
    expect(third.request.previous_client_message_id).toBe('message-002');
  });

  it('isolates timelines by account, service, project and conversation', async () => {
    const storage = new MemoryOutbox();
    const base = entry({ client_message_id: 'personal-001', content: '一' }, []);
    await enqueueOutboxMessage(storage, 'account-service', base);
    const cases = [
      ['another-service', { ...base, request: { client_message_id: 'other-service', content: '二' } }],
      ['account-service', { ...base, actorID: 'u7654321', request: { client_message_id: 'other-actor', content: '二' } }],
      ['account-service', { ...base, projectID: 'j1234567', request: { client_message_id: 'project', content: '二' } }],
      ['account-service', { ...base, conversationID: 'c7654321', request: { client_message_id: 'other-conversation', content: '二' } }],
    ] as const;
    for (const [scope, message] of cases) {
      const saved = await enqueueOutboxMessage(storage, scope, message);
      expect(saved.request.previous_client_message_id).toBeUndefined();
    }
  });

  it('joins a new-conversation key to the real conversation and preserves the queued chain', async () => {
    const storage = new MemoryOutbox();
    const draft = { ...entry({ client_message_id: 'draft-001', content: '一' }, []), conversationID: null, conversationCreateKey: 'create-001' };
    await enqueueOutboxMessage(storage, 'account-service', draft);
    await bindOutboxConversation(storage, 'account-service', draft.actorID, 'create-001', 'c1234567');
    const [byConversation, byDraft] = await Promise.all([
      enqueueOutboxMessage(storage, 'account-service', { ...draft, conversationID: 'c1234567', conversationCreateKey: undefined,
        request: { client_message_id: 'conversation-002', content: '二' } }),
      enqueueOutboxMessage(storage, 'account-service', { ...draft, request: { client_message_id: 'draft-003', content: '三' } }),
    ]);
    expect(byConversation.request.previous_client_message_id).toBe('draft-001');
    expect(byDraft.request.previous_client_message_id).toBe('conversation-002');
    await bindOutboxConversation(storage, 'account-service', draft.actorID, 'create-001', 'c1234567');
    expect((await storage.read('account-service')).every((item) => item.conversationID === 'c1234567')).toBe(true);
  });

  it('retains both messages when a new-conversation mapping conflicts with an existing timeline', async () => {
    const storage = new MemoryOutbox();
    const draft = { ...entry({ client_message_id: 'draft-001', content: '一' }, []), conversationID: null, conversationCreateKey: 'create-001' };
    await enqueueOutboxMessage(storage, 'account-service', draft);
    await enqueueOutboxMessage(storage, 'account-service', entry({ client_message_id: 'other-001', content: '二' }, []));
    await expect(bindOutboxConversation(storage, 'account-service', draft.actorID, 'create-001', 'c1234567'))
      .rejects.toThrow('本地消息顺序待核对');
    expect((await storage.read('account-service')).map((item) => item.request.client_message_id)).toEqual(['draft-001', 'other-001']);
  });
});
