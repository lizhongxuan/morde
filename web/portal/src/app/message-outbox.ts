import type { components } from '../api/schema';

export type PendingMessageAttachment = {
  id: string;
  name: string;
  mediaType: string;
  blob: Blob;
  objectID: string;
  objectKey: string;
  uploadKey: string;
  commitKey: string;
  attachmentID?: string;
  error?: string;
};

export type PendingConversationMessage = {
  request: components['schemas']['AcceptConversationMessageRequest'];
  actorID: string;
  serviceInstanceID?: string;
  conversationID: string | null;
  conversationCreateKey?: string;
  projectID?: string;
  conversationOwnerID?: string;
  messageID?: string;
  attachments: PendingMessageAttachment[];
  status: 'pending' | 'sending' | 'unknown' | 'failed' | 'accepted';
  error?: string;
  createdAt: number;
};

export type OutboxOrdering = { heads: Record<string, string>; aliases: Record<string, string> };

function timelineKey(message: Pick<PendingConversationMessage, 'actorID' | 'projectID' | 'conversationID' | 'conversationCreateKey'>): string {
  const conversation = message.conversationID ? `conversation:${message.conversationID}` : `new:${message.conversationCreateKey ?? ''}`;
  return JSON.stringify([message.actorID, message.projectID ?? null, conversation]);
}

function resolvedTimeline(ordering: OutboxOrdering, key: string): string {
  return ordering.aliases[key] ?? key;
}

/** Select the predecessor and persist both the message and timeline head in one storage transaction. */
export async function enqueueOutboxMessage(
  storage: MessageOutboxStorage, scope: string, message: PendingConversationMessage, serverPredecessor?: string,
): Promise<PendingConversationMessage> {
  let saved: PendingConversationMessage | undefined;
  await storage.update(scope, (items, ordering) => {
    const existing = items.find((item) => item.request.client_message_id === message.request.client_message_id);
    if (existing) throw new Error('消息编号已存在，原消息已保留。');
    const timeline = resolvedTimeline(ordering, timelineKey(message));
    const lastPending = items.filter((item) => resolvedTimeline(ordering, timelineKey(item)) === timeline).at(-1)?.request.client_message_id;
    const predecessor = ordering.heads[timeline] ?? lastPending ?? serverPredecessor;
    if (predecessor === message.request.client_message_id) throw new Error('消息前序编号无效，输入已保留。');
    saved = { ...message, request: { ...message.request, ...(predecessor ? { previous_client_message_id: predecessor } : {}) } };
    ordering.heads[timeline] = saved.request.client_message_id;
    return [...items, saved];
  });
  if (!saved) throw new Error('无法保存本地消息。');
  return saved;
}

/** Make messages saved while creating a conversation share the durable conversation timeline. */
export async function bindOutboxConversation(
  storage: MessageOutboxStorage, scope: string, actorID: string, createKey: string, conversationID: string,
): Promise<void> {
  await storage.update(scope, (items, ordering) => {
    const draft = timelineKey({ actorID, conversationID: null, conversationCreateKey: createKey });
    const conversation = timelineKey({ actorID, conversationID });
    const linked = ordering.aliases[draft];
    if (linked && linked !== conversation) throw new Error('新会话的本地身份不一致，消息已保留。');
    const draftHead = linked ? undefined : ordering.heads[draft] ?? items.filter((item) => item.actorID === actorID && !item.projectID
      && item.conversationCreateKey === createKey).at(-1)?.request.client_message_id;
    const conversationHead = ordering.heads[conversation];
    if (draftHead && conversationHead && draftHead !== conversationHead) {
      throw new Error('新会话的本地消息顺序待核对，消息已保留。');
    }
    ordering.aliases[draft] = conversation;
    if (draftHead) ordering.heads[conversation] = draftHead;
    delete ordering.heads[draft];
    return items.map((item) => {
      if (item.actorID !== actorID || item.projectID || item.conversationCreateKey !== createKey) return item;
      if (item.conversationID && item.conversationID !== conversationID) throw new Error('新会话的本地身份不一致，消息已保留。');
      return { ...item, conversationID };
    });
  });
}

export function outboxReceiptRequest(message: PendingConversationMessage): components['schemas']['AcceptConversationMessageRequest'] {
  const source = message.request;
  const clientAttachmentIDs = source.client_attachment_ids ?? message.attachments.map((attachment) => attachment.id);
  if (clientAttachmentIDs.length !== message.attachments.length ||
    clientAttachmentIDs.some((id) => !message.attachments.some((attachment) => attachment.id === id))) {
    throw new Error('消息附件资料不完整，原消息已保留，请核对附件。');
  }
  return {
    client_message_id: source.client_message_id, content: source.content,
    ...(source.previous_client_message_id ? { previous_client_message_id: source.previous_client_message_id } : {}),
    ...(source.reference_task_id ? { reference_task_id: source.reference_task_id } : {}),
    ...(source.model_id ? { model_id: source.model_id } : {}),
    ...(source.time_zone ? { time_zone: source.time_zone } : {}),
    ...(source.host_id ? { host_id: source.host_id } : {}),
    ...(source.workspace_id ? { workspace_id: source.workspace_id } : {}),
    ...(source.change_task_target ? { change_task_target: true } : {}),
    ...(source.application_id ? { application_id: source.application_id } : {}),
    ...(source.mcp_connector_ids ? { mcp_connector_ids: source.mcp_connector_ids } : {}),
    ...(clientAttachmentIDs.length ? { client_attachment_ids: clientAttachmentIDs } : {}),
    ...(source.budget ? { budget: source.budget } : {}),
  };
}

export interface MessageOutboxStorage {
  read(scope: string): Promise<PendingConversationMessage[]>;
  update(scope: string, change: (items: PendingConversationMessage[], ordering: OutboxOrdering) => PendingConversationMessage[]): Promise<PendingConversationMessage[]>;
}

const databaseName = 'morde-conversation-outbox-v1';
const storeName = 'account-state';

function openDatabase(): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') return Promise.reject(new Error('此浏览器不支持离线消息存储。'));
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1);
    request.onupgradeneeded = () => {
      if (!request.result.objectStoreNames.contains(storeName)) request.result.createObjectStore(storeName);
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('无法打开本地消息存储。'));
    request.onblocked = () => reject(new Error('本地消息存储正在升级，请关闭其他 Morde 标签页后重试。'));
  });
}

export class IndexedDBMessageOutbox implements MessageOutboxStorage {
  async read(scope: string): Promise<PendingConversationMessage[]> {
    const db = await openDatabase();
    try {
      return await new Promise((resolve, reject) => {
        const transaction = db.transaction(storeName, 'readonly');
        const request = transaction.objectStore(storeName).get(scope);
        request.onsuccess = () => resolve((request.result as PendingConversationMessage[] | undefined) ?? []);
        request.onerror = () => reject(request.error ?? new Error('无法读取本地消息。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('无法读取本地消息。'));
      });
    } finally {
      db.close();
    }
  }

  async update(scope: string, change: (items: PendingConversationMessage[], ordering: OutboxOrdering) => PendingConversationMessage[]): Promise<PendingConversationMessage[]> {
    const db = await openDatabase();
    try {
      return await new Promise((resolve, reject) => {
        const transaction = db.transaction(storeName, 'readwrite');
        const store = transaction.objectStore(storeName);
        const request = store.get(scope);
        const orderingKey = `${scope}\u0000message-order-v1`;
        const orderingRequest = store.get(orderingKey);
        let result: PendingConversationMessage[] = [];
        let itemsReady = false;
        let orderingReady = false;
        const save = () => {
          if (!itemsReady || !orderingReady) return;
          try {
            const ordering = (orderingRequest.result as OutboxOrdering | undefined) ?? { heads: {}, aliases: {} };
            result = change((request.result as PendingConversationMessage[] | undefined) ?? [], ordering);
            store.put(result, scope);
            store.put(ordering, orderingKey);
          } catch (error) {
            transaction.abort();
            reject(error);
          }
        };
        request.onsuccess = () => { itemsReady = true; save(); };
        orderingRequest.onsuccess = () => { orderingReady = true; save(); };
        request.onerror = () => reject(request.error ?? new Error('无法保存本地消息。'));
        orderingRequest.onerror = () => reject(orderingRequest.error ?? new Error('无法保存本地消息顺序。'));
        transaction.oncomplete = () => resolve(result);
        transaction.onerror = () => reject(transaction.error ?? new Error('无法保存本地消息。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('本地消息保存已取消。'));
      });
    } finally {
      db.close();
    }
  }
}

export function messageOutboxScope(userID: string, serviceInstanceID: string | undefined): string {
  return `${window.location.origin}\u0000${userID}\u0000${serviceInstanceID ?? 'unknown'}`;
}

export const messageOutbox: MessageOutboxStorage = new IndexedDBMessageOutbox();
