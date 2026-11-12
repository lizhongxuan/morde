export type AssistantComposerDraftAttachment = {
  id: string;
  name: string;
  displayName?: string;
  mediaType: string;
  blob: Blob;
  objectID: string;
  objectKey: string;
  uploadKey: string;
  commitKey: string;
  attachmentID?: string;
  previewText?: string;
  needsPreparation?: boolean;
  preparationCancelled?: boolean;
};

export type AssistantComposerDraft = {
  id: string;
  conversationID: string | null;
  content: string;
  modelID: string;
  hostID: string;
  mcpConnectorIDs: string[];
  attachments: AssistantComposerDraftAttachment[];
  updatedAt: number;
};

export interface AssistantDraftStorage {
  get(scope: string, id: string): Promise<AssistantComposerDraft | null>;
  list(scope: string): Promise<AssistantComposerDraft[]>;
  put(scope: string, draft: AssistantComposerDraft): Promise<void>;
  remove(scope: string, id: string): Promise<void>;
}

const databaseName = 'morde-assistant-drafts-v1';
const storeName = 'drafts';

function openDatabase(): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') return Promise.reject(new Error('此浏览器不支持本地对话草稿存储。'));
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(databaseName, 1);
    request.onupgradeneeded = () => {
      const store = request.result.createObjectStore(storeName, { keyPath: 'key' });
      store.createIndex('scope', 'scope');
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('无法打开本地对话草稿。'));
    request.onblocked = () => reject(new Error('本地草稿存储正在升级，请关闭其他 Morde 标签页后重试。'));
  });
}

export function assistantComposerDraftScope(serviceInstanceID: string, userID: string, origin = window.location.origin): string {
  return JSON.stringify([origin, serviceInstanceID, userID]);
}

export class IndexedDBAssistantDraftStorage implements AssistantDraftStorage {
  async get(scope: string, id: string): Promise<AssistantComposerDraft | null> {
    const database = await openDatabase();
    try {
      return await new Promise((resolve, reject) => {
        const request = database.transaction(storeName).objectStore(storeName).get(`${scope}\u0000${id}`);
        request.onsuccess = () => resolve((request.result as { draft: AssistantComposerDraft } | undefined)?.draft ?? null);
        request.onerror = () => reject(request.error ?? new Error('无法读取本地对话草稿。'));
      });
    } finally {
      database.close();
    }
  }

  async list(scope: string): Promise<AssistantComposerDraft[]> {
    const database = await openDatabase();
    try {
      return await new Promise((resolve, reject) => {
        const request = database.transaction(storeName).objectStore(storeName).index('scope').getAll(scope);
        request.onsuccess = () => resolve((request.result as { draft: AssistantComposerDraft }[]).map((item) => item.draft));
        request.onerror = () => reject(request.error ?? new Error('无法列出本地对话草稿。'));
      });
    } finally {
      database.close();
    }
  }

  async put(scope: string, draft: AssistantComposerDraft): Promise<void> {
    const database = await openDatabase();
    try {
      await new Promise<void>((resolve, reject) => {
        const transaction = database.transaction(storeName, 'readwrite');
        transaction.objectStore(storeName).put({ key: `${scope}\u0000${draft.id}`, scope, draft });
        transaction.oncomplete = () => resolve();
        transaction.onerror = () => reject(transaction.error ?? new Error('无法保存本地对话草稿。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('本地对话草稿保存已取消。'));
      });
    } finally {
      database.close();
    }
  }

  async remove(scope: string, id: string): Promise<void> {
    const database = await openDatabase();
    try {
      await new Promise<void>((resolve, reject) => {
        const transaction = database.transaction(storeName, 'readwrite');
        transaction.objectStore(storeName).delete(`${scope}\u0000${id}`);
        transaction.oncomplete = () => resolve();
        transaction.onerror = () => reject(transaction.error ?? new Error('无法删除本地对话草稿。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('本地对话草稿操作已取消。'));
      });
    } finally {
      database.close();
    }
  }
}

/**
 * Keep edits recoverable for the lifetime of the page even when IndexedDB is
 * unavailable or out of space. Persistence errors are still surfaced to the
 * caller so the UI can explain that a refresh may not retain the draft.
 */
export class ResilientAssistantDraftStorage implements AssistantDraftStorage {
  private readonly memory = new Map<string, AssistantComposerDraft>();

  constructor(private readonly durable: AssistantDraftStorage) {}

  async get(scope: string, id: string): Promise<AssistantComposerDraft | null> {
    try {
      const draft = await this.durable.get(scope, id);
      if (draft) this.memory.set(`${scope}\u0000${id}`, draft);
      return draft ?? this.memory.get(`${scope}\u0000${id}`) ?? null;
    } catch (error) {
      const draft = this.memory.get(`${scope}\u0000${id}`);
      if (draft) return draft;
      throw error;
    }
  }

  async list(scope: string): Promise<AssistantComposerDraft[]> {
    try {
      const durable = await this.durable.list(scope);
      for (const draft of durable) this.memory.set(`${scope}\u0000${draft.id}`, draft);
      const byID = new Map(durable.map((draft) => [draft.id, draft]));
      for (const [key, draft] of this.memory) {
        if (key.startsWith(`${scope}\u0000`)) byID.set(draft.id, draft);
      }
      return [...byID.values()];
    } catch (error) {
      const fallback = [...this.memory].filter(([key]) => key.startsWith(`${scope}\u0000`)).map(([, draft]) => draft);
      if (fallback.length) return fallback;
      throw error;
    }
  }

  async put(scope: string, draft: AssistantComposerDraft): Promise<void> {
    this.memory.set(`${scope}\u0000${draft.id}`, draft);
    await this.durable.put(scope, draft);
  }

  async remove(scope: string, id: string): Promise<void> {
    this.memory.delete(`${scope}\u0000${id}`);
    await this.durable.remove(scope, id);
  }
}

export const assistantDraftStorage: AssistantDraftStorage = new ResilientAssistantDraftStorage(new IndexedDBAssistantDraftStorage());
