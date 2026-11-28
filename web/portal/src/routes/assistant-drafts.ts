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

export function assistantComposerDraftIdentity(scope: string, id: string): string {
  return JSON.stringify([scope, id]);
}

export type AssistantDraftPreviewAttachment = Pick<AssistantComposerDraftAttachment, 'id' | 'mediaType' | 'blob'> & {
  needsPreparation?: boolean;
};

/** Owns local image URLs for one active draft; URLs are never persisted with the draft. */
export class AssistantDraftPreviewURLRegistry {
  private identity: string | null = null;
  private readonly urls = new Map<string, { blob: Blob; url: string }>();

  constructor(
    private readonly createURL: (blob: Blob) => string = (blob) =>
      typeof URL.createObjectURL === 'function' ? URL.createObjectURL(blob) : '',
    private readonly revokeURL: (url: string) => void = (url) => {
      if (typeof URL.revokeObjectURL === 'function') URL.revokeObjectURL(url);
    },
  ) {}

  sync(identity: string | null, attachments: AssistantDraftPreviewAttachment[]): Record<string, string> {
    if (!identity) {
      this.clear();
      return {};
    }
    if (identity !== this.identity) {
      this.releaseAll();
      this.identity = identity;
    }

    const previewable = new Map(attachments.filter((item) => !item.needsPreparation &&
      (item.mediaType === 'image/png' || item.mediaType === 'image/jpeg')).map((item) => [item.id, item.blob]));
    for (const [id, current] of this.urls) {
      const blob = previewable.get(id);
      if (blob !== current.blob) {
        this.revokeURL(current.url);
        this.urls.delete(id);
      }
    }
    for (const [id, blob] of previewable) {
      if (this.urls.has(id)) continue;
      try {
        const url = this.createURL(blob);
        if (url) this.urls.set(id, { blob, url });
      } catch {
        // A preview is optional; attachment staging and sending remain available.
      }
    }
    return Object.fromEntries([...this.urls].map(([id, current]) => [id, current.url]));
  }

  clear(): void {
    this.releaseAll();
    this.identity = null;
  }

  private releaseAll(): void {
    for (const current of this.urls.values()) this.revokeURL(current.url);
    this.urls.clear();
  }
}

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
  private readonly dirty = new Map<string, AssistantComposerDraft>();
  private readonly removed = new Set<string>();
  private readonly revisions = new Map<string, number>();
  private readonly writes = new Map<string, Promise<void>>();

  constructor(private readonly durable: AssistantDraftStorage) {}

  private write(key: string, operation: () => Promise<void>): Promise<void> {
    const previous = this.writes.get(key) ?? Promise.resolve();
    const write = previous.catch(() => undefined).then(operation);
    this.writes.set(key, write);
    const clear = () => { if (this.writes.get(key) === write) this.writes.delete(key); };
    void write.then(clear, clear);
    return write;
  }

  async get(scope: string, id: string): Promise<AssistantComposerDraft | null> {
    const key = `${scope}\u0000${id}`;
    const before = this.revisions.get(key);
    try {
      const draft = await this.durable.get(scope, id);
      const cached = this.memory.get(key);
      if (this.removed.has(key)) return null;
      if (this.dirty.has(key) || this.revisions.get(key) !== before) return cached ?? null;
      if (draft) this.memory.set(key, draft);
      return draft ?? cached ?? null;
    } catch (error) {
      if (this.removed.has(key)) return null;
      const draft = this.memory.get(key);
      if (draft) return draft;
      throw error;
    }
  }

  async list(scope: string): Promise<AssistantComposerDraft[]> {
    const before = new Map(this.revisions);
    try {
      const durable = await this.durable.list(scope);
      for (const draft of durable) {
        const key = `${scope}\u0000${draft.id}`;
        if (!this.removed.has(key) && !this.dirty.has(key) && this.revisions.get(key) === before.get(key)) this.memory.set(key, draft);
      }
      const byID = new Map<string, AssistantComposerDraft>();
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
    const key = `${scope}\u0000${draft.id}`;
    this.revisions.set(key, (this.revisions.get(key) ?? 0) + 1);
    this.removed.delete(key);
    this.memory.set(key, draft);
    this.dirty.set(key, draft);
    await this.write(key, () => this.durable.put(scope, draft));
    if (this.dirty.get(key) === draft) this.dirty.delete(key);
  }

  async remove(scope: string, id: string): Promise<void> {
    const key = `${scope}\u0000${id}`;
    const previous = this.memory.get(key);
    const previousDirty = this.dirty.get(key);
    const revision = (this.revisions.get(key) ?? 0) + 1;
    this.revisions.set(key, revision);
    this.removed.add(key);
    this.memory.delete(key);
    this.dirty.delete(key);
    try {
      await this.write(key, () => this.durable.remove(scope, id));
    } catch (error) {
      if (this.revisions.get(key) === revision) {
        this.revisions.set(key, revision + 1);
        this.removed.delete(key);
        if (previous) this.memory.set(key, previous);
        if (previousDirty) this.dirty.set(key, previousDirty);
      }
      throw error;
    }
  }
}

export const assistantDraftStorage: AssistantDraftStorage = new ResilientAssistantDraftStorage(new IndexedDBAssistantDraftStorage());
