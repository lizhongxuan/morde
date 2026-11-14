import type { CreateMessageVariantInput } from './message-variants';

export type PendingMessageVariantOperation = {
  input: CreateMessageVariantInput;
  status: 'pending' | 'sending' | 'unknown' | 'failed';
  error?: string;
  createdAt: number;
};

export interface MessageVariantOutboxStorage {
  read(scope: string): Promise<PendingMessageVariantOperation[]>;
  update(scope: string, change: (items: PendingMessageVariantOperation[]) => PendingMessageVariantOperation[]): Promise<PendingMessageVariantOperation[]>;
}

const databaseName = 'morde-conversation-outbox-v1';
const storeName = 'account-state';

function openDatabase(): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') return Promise.reject(new Error('此浏览器不支持离线消息版本操作存储。'));
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1);
    request.onupgradeneeded = () => {
      if (!request.result.objectStoreNames.contains(storeName)) request.result.createObjectStore(storeName);
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('无法打开本地消息版本操作存储。'));
    request.onblocked = () => reject(new Error('本地消息版本操作存储正在升级，请关闭其他 Morde 标签页后重试。'));
  });
}

function storageKey(scope: string): string { return `${scope}\u0000message-variant-operations-v1`; }

export class IndexedDBMessageVariantOutbox implements MessageVariantOutboxStorage {
  async read(scope: string): Promise<PendingMessageVariantOperation[]> {
    const db = await openDatabase();
    try {
      return await new Promise((resolve, reject) => {
        const transaction = db.transaction(storeName, 'readonly');
        const request = transaction.objectStore(storeName).get(storageKey(scope));
        request.onsuccess = () => resolve((request.result as PendingMessageVariantOperation[] | undefined) ?? []);
        request.onerror = () => reject(request.error ?? new Error('无法读取本地消息版本操作。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('无法读取本地消息版本操作。'));
      });
    } finally { db.close(); }
  }

  async update(scope: string, change: (items: PendingMessageVariantOperation[]) => PendingMessageVariantOperation[]): Promise<PendingMessageVariantOperation[]> {
    const db = await openDatabase();
    try {
      return await new Promise((resolve, reject) => {
        const transaction = db.transaction(storeName, 'readwrite');
        const store = transaction.objectStore(storeName);
        const request = store.get(storageKey(scope));
        let result: PendingMessageVariantOperation[] = [];
        request.onsuccess = () => {
          try {
            result = change((request.result as PendingMessageVariantOperation[] | undefined) ?? []);
            store.put(result, storageKey(scope));
          } catch (error) {
            transaction.abort();
            reject(error);
          }
        };
        request.onerror = () => reject(request.error ?? new Error('无法保存本地消息版本操作。'));
        transaction.oncomplete = () => resolve(result);
        transaction.onerror = () => reject(transaction.error ?? new Error('无法保存本地消息版本操作。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('本地消息版本操作保存已取消。'));
      });
    } finally { db.close(); }
  }
}

export function messageVariantOutboxScope(actorID: string, serviceInstanceID: string | undefined): string {
  return `${window.location.origin}\u0000${actorID}\u0000${serviceInstanceID ?? 'unknown'}`;
}

export const messageVariantOutbox: MessageVariantOutboxStorage = new IndexedDBMessageVariantOutbox();
