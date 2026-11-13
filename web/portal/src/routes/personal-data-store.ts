import type { components } from '../api/schema';

export type PersonalObjectType = components['schemas']['PersonalObjectType'];
export type PersonalObjectBody = Record<string, unknown>;

export interface PersonalObject {
  object_type: PersonalObjectType;
  object_id: string;
  revision: number;
  body: PersonalObjectBody;
}

export type PersonalMutation = {
  id: string;
  key: string;
  objectType: PersonalObjectType;
  objectID: string;
  baseRevision: number;
  body: PersonalObjectBody | null;
  operation: 'replace' | 'delete';
  status: 'pending' | 'conflict';
  createdAt: number;
  message?: string;
  resolutionQueued?: boolean;
  migrationSource?: { scope: string; id: string };
};

export interface PendingAttachment {
  id: string;
  objectID: string;
  name: string;
  kind: 'artifact' | 'media';
  mediaType: string;
  sizeBytes: number;
  sha256: string;
  blob: Blob;
  baseRevision: number;
  dependsOnKey?: string;
  createKey: string;
  commitKey: string;
  uploadID?: string;
  status: 'pending' | 'conflict';
  message?: string;
  cancelUploadPending?: boolean;
  resolutionQueued?: boolean;
  migrationSource?: { scope: string; id: string };
}

export interface CachedAttachment {
  attachmentID: string;
  name: string;
  mediaType: string;
  blob: Blob;
}

export interface PersonalSyncCursor {
  initialized: boolean;
  afterSequence: number;
}

export interface PersonalDataState {
  objects: Record<string, PersonalObject>;
  outbox: PersonalMutation[];
  attachments: PendingAttachment[];
  cachedAttachments: CachedAttachment[];
  cursor: PersonalSyncCursor;
  snapshotComplete: boolean;
  syncError: string | null;
  lastSyncedAt: number | null;
}

export interface PersonalDataSyncStatus {
  pendingCount: number | null;
  conflictCount: number | null;
  error?: string;
}

export interface PersonalDataUploadRequest {
  object_id: string;
  base_revision: number;
  size_bytes: number;
  sha256: string;
  media_type: string;
}

export interface PersonalSnapshotPage {
  watermark: number;
  items: PersonalObject[];
  next_cursor: string | null;
}

export interface PersonalChangePage {
  items: Array<{
    change_sequence: number;
    object_type: PersonalObjectType;
    object_id: string;
    revision: number;
    operation: 'upsert' | 'delete';
    mutation_id?: string;
  }>;
  through_sequence: number;
  next_after_sequence: number;
  has_more: boolean;
}

/** The Portal API methods used here. Kept narrow so the sync engine can be tested in isolation. */
export interface PersonalDataAPI {
  personalDataSnapshot(cursor?: string, limit?: number): Promise<PersonalSnapshotPage>;
  personalDataChanges(after: number, through?: number, limit?: number): Promise<PersonalChangePage>;
  getPersonalObject(type: PersonalObjectType, id: string): Promise<PersonalObject>;
  replacePersonalObject(type: PersonalObjectType, id: string, baseRevision: number, body: PersonalObjectBody, key?: string): Promise<{ result_revision: number; account_change_sequence: number }>;
  deletePersonalObject(type: PersonalObjectType, id: string, baseRevision: number, key?: string): Promise<{ result_revision: number; account_change_sequence: number }>;
  personalDataQuota(): Promise<{ used_bytes: number; reserved_bytes: number; quota_bytes: number; remaining_bytes: number; max_attachment_bytes: number }>;
  createPersonalAttachmentUpload(body: PersonalDataUploadRequest, key?: string): Promise<{ upload_id: string; state: string }>;
  putPersonalAttachmentChunk(uploadID: string, sequence: number, body: BodyInit, sha256: string): Promise<unknown>;
  commitPersonalAttachment(uploadID: string, key?: string): Promise<{ attachment_id: string; result_revision: number; object_id: string }>;
  cancelPersonalAttachmentUpload(uploadID: string): Promise<void>;
  personalAttachment(attachmentID: string): Promise<Blob>;
}

export interface PersonalDataStorage {
  read(scope: string): Promise<PersonalDataState>;
  update(scope: string, change: (current: PersonalDataState) => PersonalDataState): Promise<PersonalDataState>;
  listScopes(prefix: string): Promise<string[]>;
}

const DATABASE_NAME = 'morde-personal-data-v1';
const DATABASE_VERSION = 1;
const STATE_STORE = 'account-state';
const EMPTY_STATE: PersonalDataState = {
  objects: {}, outbox: [], attachments: [], cachedAttachments: [],
  cursor: { initialized: false, afterSequence: 0 }, snapshotComplete: false,
  syncError: null, lastSyncedAt: null,
};

function copyState(value: PersonalDataState): PersonalDataState {
  return {
    ...structuredClone({ ...value, attachments: [], cachedAttachments: [] }),
    attachments: value.attachments.map(copyPendingAttachment),
    cachedAttachments: value.cachedAttachments.map(copyCachedAttachment),
  };
}

function copyPendingAttachment(value: PendingAttachment): PendingAttachment {
  const { blob, ...metadata } = value;
  return { ...structuredClone(metadata), blob };
}

function copyCachedAttachment(value: CachedAttachment): CachedAttachment {
  const { blob, ...metadata } = value;
  return { ...structuredClone(metadata), blob };
}

function normalizedState(value: Partial<PersonalDataState> | undefined): PersonalDataState {
  return {
    ...EMPTY_STATE,
    ...value,
    objects: value?.objects ?? {},
    outbox: value?.outbox ?? [],
    attachments: value?.attachments ?? [],
    cachedAttachments: value?.cachedAttachments ?? [],
    cursor: value?.cursor ?? { initialized: false, afterSequence: 0 },
  };
}

function database(): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') return Promise.reject(new Error('此浏览器不支持离线数据存储。'));
  return new Promise((resolve, reject) => {
    const request = indexedDB.open(DATABASE_NAME, DATABASE_VERSION);
    request.onupgradeneeded = () => {
      if (!request.result.objectStoreNames.contains(STATE_STORE)) request.result.createObjectStore(STATE_STORE);
    };
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('无法打开离线数据存储。'));
    request.onblocked = () => reject(new Error('离线数据存储正在升级，请关闭其他 Morde 标签页后重试。'));
  });
}

export class IndexedDBPersonalDataStorage implements PersonalDataStorage {
  async read(scope: string): Promise<PersonalDataState> {
    const db = await database();
    try {
      return await new Promise((resolve, reject) => {
        const transaction = db.transaction(STATE_STORE, 'readonly');
        const request = transaction.objectStore(STATE_STORE).get(scope);
        request.onsuccess = () => resolve(copyState(normalizedState(request.result as Partial<PersonalDataState> | undefined)));
        request.onerror = () => reject(request.error ?? new Error('无法读取本地个人数据。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('无法读取本地个人数据。'));
      });
    } finally {
      db.close();
    }
  }

  async update(scope: string, change: (current: PersonalDataState) => PersonalDataState): Promise<PersonalDataState> {
    const db = await database();
    try {
      return await new Promise((resolve, reject) => {
        const transaction = db.transaction(STATE_STORE, 'readwrite');
        const objectStore = transaction.objectStore(STATE_STORE);
        let result: PersonalDataState | undefined;
        const request = objectStore.get(scope);
        request.onsuccess = () => {
          try {
            result = copyState(change(normalizedState(request.result as Partial<PersonalDataState> | undefined)));
            objectStore.put(result, scope);
          } catch (error) {
            transaction.abort();
            reject(error);
          }
        };
        request.onerror = () => reject(request.error ?? new Error('无法更新本地个人数据。'));
        transaction.oncomplete = () => resolve(result ?? EMPTY_STATE);
        transaction.onerror = () => reject(transaction.error ?? new Error('无法更新本地个人数据。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('本地个人数据更新已取消。'));
      });
    } finally {
      db.close();
    }
  }

  async listScopes(prefix: string): Promise<string[]> {
    const db = await database();
    try {
      return await new Promise((resolve, reject) => {
        const transaction = db.transaction(STATE_STORE, 'readonly');
        const request = transaction.objectStore(STATE_STORE).getAllKeys();
        request.onsuccess = () => resolve(request.result.filter((key): key is string => typeof key === 'string' && key.startsWith(prefix)));
        request.onerror = () => reject(request.error ?? new Error('无法列出本地个人数据草稿。'));
        transaction.onabort = () => reject(transaction.error ?? new Error('无法列出本地个人数据草稿。'));
      });
    } finally {
      db.close();
    }
  }
}

export function createMemoryPersonalDataStorage(): PersonalDataStorage {
  const values = new Map<string, PersonalDataState>();
  const tails = new Map<string, Promise<unknown>>();
  return {
    async read(scope) { return copyState(values.get(scope) ?? EMPTY_STATE); },
    async update(scope, change) {
      const previous = tails.get(scope) ?? Promise.resolve();
      let release!: () => void;
      const current = new Promise<void>((resolve) => { release = resolve; });
      const tail = previous.then(() => current);
      tails.set(scope, tail);
      await previous;
      try {
        const next = copyState(change(copyState(values.get(scope) ?? EMPTY_STATE)));
        values.set(scope, next);
        return copyState(next);
      } finally {
        release();
        if (tails.get(scope) === tail) tails.delete(scope);
      }
    },
    async listScopes(prefix) { return [...values.keys()].filter((scope) => scope.startsWith(prefix)); },
  };
}

export function personalDataScopeKey(serviceOrigin: string, userID: string): string {
  return `${new URL(serviceOrigin).origin}\u0000${userID}\u0000unconfirmed`;
}

export function personalDataInstanceScopeKey(serviceOrigin: string, userID: string, serviceInstanceID: string): string {
  if (!serviceInstanceID) throw new Error('服务实例尚未确认，不能创建已确认的数据范围。');
  return `${new URL(serviceOrigin).origin}\u0000${userID}\u0000${encodeURIComponent(serviceInstanceID)}`;
}

export function personalDataLegacyScopeKey(serviceOrigin: string, userID: string): string {
  return `${new URL(serviceOrigin).origin}\u0000${userID}`;
}

export function personalObjectKey(type: PersonalObjectType, id: string): string {
  return JSON.stringify([type, id]);
}

const statusListeners = new Map<string, Set<(status: PersonalDataSyncStatus) => void>>();

export async function getPersonalDataSyncStatus(userID: string, serviceOrigin = currentServiceOrigin(), serviceInstanceID?: string): Promise<PersonalDataSyncStatus> {
  const origin = new URL(serviceOrigin).origin;
  const state = await new PersonalDataStore(userID, origin, undefined, serviceInstanceID).read();
  return syncStatus(state);
}

export function subscribePersonalDataSyncStatus(
  userID: string,
  listener: (status: PersonalDataSyncStatus) => void,
  serviceOrigin = currentServiceOrigin(),
  serviceInstanceID?: string,
): () => void {
  const origin = new URL(serviceOrigin).origin;
  const store = new PersonalDataStore(userID, origin, undefined, serviceInstanceID);
  const listeners = statusListeners.get(store.scope) ?? new Set<(status: PersonalDataSyncStatus) => void>();
  listeners.add(listener);
  statusListeners.set(store.scope, listeners);
  void getPersonalDataSyncStatus(userID, origin, serviceInstanceID).then(listener).catch(() => listener(syncStatusUnavailable()));
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) statusListeners.delete(store.scope);
  };
}

function currentServiceOrigin(): string {
  return typeof location === 'undefined' ? 'http://localhost' : location.origin;
}

export class PersonalDataStore {
  readonly userID: string;
  readonly serviceOrigin: string;
  readonly scope: string;
  readonly serviceInstanceID: string | undefined;
  private readonly scopeBase: string;
  private readonly storage: PersonalDataStorage;
  private readonly listeners = new Set<(state: PersonalDataState) => void>();

  constructor(userID: string, serviceOrigin = currentServiceOrigin(), storage: PersonalDataStorage = new IndexedDBPersonalDataStorage(), serviceInstanceID?: string) {
    if (!userID) throw new Error('当前用户不可用，无法隔离个人数据。');
    this.userID = userID;
    this.serviceOrigin = new URL(serviceOrigin).origin;
    this.serviceInstanceID = serviceInstanceID || undefined;
    this.scopeBase = `${this.serviceOrigin}\u0000${userID}`;
    this.scope = this.serviceInstanceID
      ? personalDataInstanceScopeKey(this.serviceOrigin, userID, this.serviceInstanceID)
      : personalDataScopeKey(this.serviceOrigin, userID);
    this.storage = storage;
  }

  async read(): Promise<PersonalDataState> { return this.storage.read(this.scope); }

  async otherInstanceDraftScopes(): Promise<Array<{ scope: string; serviceInstanceID: string | null; state: PersonalDataState }>> {
    const keys = new Set(await this.storage.listScopes(`${this.scopeBase}\u0000`));
    keys.add(this.scopeBase); // pre-instance storage key
    const results: Array<{ scope: string; serviceInstanceID: string | null; state: PersonalDataState }> = [];
    for (const scope of keys) {
      if (scope === this.scope) continue;
      const state = await this.storage.read(scope);
      if (!state.outbox.length && !state.attachments.length) continue;
      const suffix = scope.startsWith(`${this.scopeBase}\u0000`) ? scope.slice(this.scopeBase.length + 1) : null;
      let serviceInstanceID: string | null = null;
      if (suffix && suffix !== 'unconfirmed') {
        try { serviceInstanceID = decodeURIComponent(suffix); } catch { /* Keep malformed legacy scope quarantined as unknown. */ }
      }
      results.push({ scope, serviceInstanceID, state });
    }
    return results.sort((left, right) => left.scope.localeCompare(right.scope));
  }

  async quarantineOtherInstanceDrafts(): Promise<void> {
    for (const candidate of await this.otherInstanceDraftScopes()) {
      await this.storage.update(candidate.scope, (state) => {
        for (const mutation of state.outbox) {
          if (mutation.status !== 'pending') continue;
          mutation.status = 'conflict';
          mutation.message = '此草稿来自其他或未确认的服务实例；已保留，需明确选择后才能提交到当前服务。';
        }
        for (const attachment of state.attachments) {
          if (attachment.status !== 'pending') continue;
          attachment.status = 'conflict';
          attachment.message = '此附件草稿来自其他或未确认的服务实例；已保留，需明确选择后才能提交到当前服务。';
        }
        return state;
      });
    }
  }

  async importOtherInstanceMutation(sourceScope: string, mutationID: string): Promise<PersonalMutation> {
    if (!this.serviceInstanceID) throw new Error('当前服务实例尚未确认，不能处理旧草稿。');
    if (!this.isOtherInstanceScope(sourceScope)) throw new Error('草稿不属于当前账号的其他服务范围。');
    const source = (await this.storage.read(sourceScope)).outbox.find((item) => item.id === mutationID);
    if (!source || source.status !== 'conflict') throw new Error('找不到已保留的旧服务草稿。');
    const imported = await this.change((state) => {
      if (!state.snapshotComplete) throw new Error('尚未读取当前服务的确认版本；请联网后重试。');
      const existing = state.outbox.find((item) => item.migrationSource?.scope === sourceScope && item.migrationSource.id === mutationID);
      if (existing) return state;
      if (source.operation === 'delete' && !state.objects[personalObjectKey(source.objectType, source.objectID)]) {
        throw new Error('当前服务中不存在此对象，未创建删除任务；旧服务删除草稿仍已保留。');
      }
      if (state.outbox.some((item) => item.objectType === source.objectType && item.objectID === source.objectID)) {
        throw new Error('当前服务已有同一对象的草稿，请先处理后再迁入旧草稿。');
      }
      state.outbox.push({
        ...structuredClone(source), id: randomID(), key: randomID(),
        baseRevision: state.objects[personalObjectKey(source.objectType, source.objectID)]?.revision ?? 0,
        status: 'pending', createdAt: Date.now(), message: undefined, resolutionQueued: undefined,
        migrationSource: { scope: sourceScope, id: mutationID },
      });
      return state;
    }, (state) => state.outbox.find((item) => item.migrationSource?.scope === sourceScope && item.migrationSource.id === mutationID)!);
    await this.storage.update(sourceScope, (state) => {
      const previous = state.outbox.find((item) => item.id === mutationID);
      if (previous) { previous.resolutionQueued = true; previous.message = '已在当前服务创建一条待提交副本；本草稿仍保留在原服务范围。'; }
      return state;
    });
    return imported;
  }

  async importOtherInstanceAttachment(sourceScope: string, attachmentID: string): Promise<PendingAttachment> {
    if (!this.serviceInstanceID) throw new Error('当前服务实例尚未确认，不能处理旧草稿。');
    if (!this.isOtherInstanceScope(sourceScope)) throw new Error('附件不属于当前账号的其他服务范围。');
    const sourceState = await this.storage.read(sourceScope);
    const source = sourceState.attachments.find((item) => item.id === attachmentID);
    if (!source || source.status !== 'conflict') throw new Error('找不到已保留的旧附件草稿。');
    const alreadyImported = (await this.read()).attachments.find((item) => item.migrationSource?.scope === sourceScope && item.migrationSource.id === attachmentID);
    if (alreadyImported) return alreadyImported;
    if (source.cancelUploadPending) throw new Error('旧服务的上传仍待取消；先保留此附件，不会在新服务重复上传。');
    const imported = await this.change((state) => {
      if (!state.snapshotComplete) throw new Error('尚未读取当前服务的确认版本；请联网后重试。');
      const existing = state.attachments.find((item) => item.migrationSource?.scope === sourceScope && item.migrationSource.id === attachmentID);
      if (existing) return state;
      let dependsOnKey: string | undefined;
      if (source.dependsOnKey) {
        const sourceMutation = sourceState.outbox.find((item) => item.key === source.dependsOnKey);
        const importedMutation = sourceMutation && state.outbox.find((item) => item.migrationSource?.scope === sourceScope && item.migrationSource.id === sourceMutation.id);
        if (!importedMutation) throw new Error('请先将附件所属的资源条目草稿迁入当前服务。');
        dependsOnKey = importedMutation.key;
      } else if (state.outbox.some((item) => item.objectType === 'library_item' && item.objectID === source.objectID)) {
        throw new Error('当前服务已有同一资源条目的草稿，请先处理后再迁入附件。');
      } else {
        const remoteItem = state.objects[personalObjectKey('library_item', source.objectID)];
        if (!remoteItem || remoteItem.body.attachment_id) throw new Error('当前服务没有可用的空资源条目；请先处理资源条目版本。');
      }
      state.attachments.push({
        ...copyPendingAttachment(source), id: randomID(), createKey: randomID(), commitKey: randomID(), uploadID: undefined,
        baseRevision: dependsOnKey ? 0 : state.objects[personalObjectKey('library_item', source.objectID)]?.revision ?? 0,
        dependsOnKey, status: 'pending', message: undefined, cancelUploadPending: undefined, resolutionQueued: undefined,
        migrationSource: { scope: sourceScope, id: attachmentID },
      });
      return state;
    }, (state) => state.attachments.find((item) => item.migrationSource?.scope === sourceScope && item.migrationSource.id === attachmentID)!);
    await this.storage.update(sourceScope, (state) => {
      const previous = state.attachments.find((item) => item.id === attachmentID);
      if (previous) { previous.resolutionQueued = true; previous.message = '已在当前服务创建一条待提交副本；原附件和上传凭据仍保留在旧服务范围。'; }
      return state;
    });
    return imported;
  }

  private isOtherInstanceScope(scope: string): boolean {
    return scope === personalDataLegacyScopeKey(this.serviceOrigin, this.userID)
      || (scope.startsWith(`${this.scopeBase}\u0000`) && scope !== this.scope);
  }

  subscribe(listener: (state: PersonalDataState) => void): () => void {
    this.listeners.add(listener);
    const scopeListeners = stateListeners.get(this.scope) ?? new Set<(state: PersonalDataState) => void>();
    scopeListeners.add(listener);
    stateListeners.set(this.scope, scopeListeners);
    return () => {
      this.listeners.delete(listener);
      scopeListeners.delete(listener);
      if (scopeListeners.size === 0) stateListeners.delete(this.scope);
    };
  }

  async queueReplace(type: PersonalObjectType, id: string, body: PersonalObjectBody): Promise<PersonalMutation> {
    return this.change((state) => {
      const key = personalObjectKey(type, id);
      if (state.outbox.some((item) => item.objectType === type && item.objectID === id && item.status === 'conflict' && !item.resolutionQueued)) {
        throw new Error('此对象有未解决的版本冲突。请先选择基于当前远端版本保存草稿。');
      }
      const previous = [...state.outbox].reverse().find((item) => item.objectType === type && item.objectID === id && item.status === 'pending');
      const mutation: PersonalMutation = {
        id: randomID(), key: randomID(), objectType: type, objectID: id,
        baseRevision: previous ? previous.baseRevision + 1 : state.objects[key]?.revision ?? 0,
        body: structuredClone(body), operation: 'replace', status: 'pending', createdAt: Date.now(),
      };
      state.outbox.push(mutation);
      return state;
    }, (state) => state.outbox[state.outbox.length - 1]!);
  }

  async queueDelete(type: PersonalObjectType, id: string): Promise<PersonalMutation> {
    return this.change((state) => {
      const key = personalObjectKey(type, id);
      if (type === 'library_item' && state.attachments.some((item) => item.objectID === id)) {
        throw new Error('此资源有待上传的附件；同步或处理附件草稿后再删除。');
      }
      if (state.outbox.some((item) => item.objectType === type && item.objectID === id && item.status === 'conflict' && !item.resolutionQueued)) {
        throw new Error('此对象有未解决的版本冲突。请先处理保留的草稿。');
      }
      const previous = [...state.outbox].reverse().find((item) => item.objectType === type && item.objectID === id && item.status === 'pending');
      state.outbox.push({
        id: randomID(), key: randomID(), objectType: type, objectID: id,
        baseRevision: previous ? previous.baseRevision + 1 : state.objects[key]?.revision ?? 0,
        body: null, operation: 'delete', status: 'pending', createdAt: Date.now(),
      });
      return state;
    }, (state) => [...state.outbox].reverse()[0]!);
  }

  async rebaseDraft(mutationID: string, bodyOverride?: PersonalObjectBody): Promise<PersonalMutation> {
    return this.change((state) => {
      const draft = state.outbox.find((item) => item.id === mutationID && item.status === 'conflict');
      if (!draft) throw new Error('找不到待处理的冲突草稿。');
      const key = personalObjectKey(draft.objectType, draft.objectID);
      const newMutation: PersonalMutation = {
        ...structuredClone(draft), id: randomID(), key: randomID(),
        baseRevision: state.objects[key]?.revision ?? 0,
        body: bodyOverride ? structuredClone(bodyOverride) : structuredClone(draft.body),
        status: 'pending', createdAt: Date.now(), message: undefined, resolutionQueued: undefined,
      };
      draft.resolutionQueued = true;
      state.outbox.push(newMutation);
      return state;
    }, (state) => state.outbox[state.outbox.length - 1]!);
  }

  async queueAttachment(input: Omit<PendingAttachment, 'id' | 'createKey' | 'commitKey' | 'uploadID' | 'status' | 'message'>): Promise<PendingAttachment> {
    return this.change((state) => {
      const key = personalObjectKey('library_item', input.objectID);
      if (state.outbox.some((item) => item.objectType === 'library_item' && item.objectID === input.objectID && item.status === 'conflict' && !item.resolutionQueued)) {
        throw new Error('此资源条目有未解决的版本冲突，暂不能上传附件。');
      }
      state.attachments.push({
        ...input, id: randomID(), createKey: randomID(), commitKey: randomID(), status: 'pending',
        baseRevision: input.dependsOnKey ? 0 : input.baseRevision ?? state.objects[key]?.revision ?? 0,
      });
      return state;
    }, (state) => state.attachments[state.attachments.length - 1]!);
  }

  async queueLibraryAttachment(
    input: Omit<PendingAttachment, 'id' | 'createKey' | 'commitKey' | 'uploadID' | 'status' | 'message' | 'baseRevision' | 'dependsOnKey'>,
    libraryBody: PersonalObjectBody,
  ): Promise<void> {
    await this.change((state) => {
      const key = personalObjectKey('library_item', input.objectID);
      if (state.outbox.some((item) => item.objectType === 'library_item' && item.objectID === input.objectID && item.status === 'conflict' && !item.resolutionQueued)) {
        throw new Error('此资源条目有未解决的版本冲突，暂不能上传附件。');
      }
      const mutation: PersonalMutation = {
        id: randomID(), key: randomID(), objectType: 'library_item', objectID: input.objectID,
        baseRevision: state.objects[key]?.revision ?? 0, body: structuredClone(libraryBody),
        operation: 'replace', status: 'pending', createdAt: Date.now(),
      };
      state.outbox.push(mutation);
      state.attachments.push({
        ...input, id: randomID(), createKey: randomID(), commitKey: randomID(), status: 'pending',
        baseRevision: 0, dependsOnKey: mutation.key,
      });
      return state;
    });
  }

  async cacheAttachment(attachment: CachedAttachment): Promise<void> {
    await this.change((state) => {
      state.cachedAttachments = [
        ...state.cachedAttachments.filter((item) => item.attachmentID !== attachment.attachmentID),
        copyCachedAttachment(attachment),
      ];
      return state;
    });
  }

  async cachedAttachment(attachmentID: string): Promise<CachedAttachment | undefined> {
    return (await this.read()).cachedAttachments.find((item) => item.attachmentID === attachmentID);
  }

  async visibleObjects(): Promise<PersonalObject[]> {
    const state = await this.read();
    const visible = new Map(Object.entries(state.objects));
    for (const entry of state.outbox) {
      const key = personalObjectKey(entry.objectType, entry.objectID);
      if (entry.operation === 'delete') visible.delete(key);
      else if (entry.body) visible.set(key, {
        object_type: entry.objectType, object_id: entry.objectID,
        revision: entry.baseRevision, body: structuredClone(entry.body),
      });
    }
    return [...visible.values()];
  }

  async applySnapshot(objects: PersonalObject[], watermark: number): Promise<void> {
    await this.change((state) => {
      const nextObjects: Record<string, PersonalObject> = {};
      for (const object of objects) nextObjects[personalObjectKey(object.object_type, object.object_id)] = structuredClone(object);
      state.objects = nextObjects;
      state.snapshotComplete = true;
      state.cursor = { initialized: true, afterSequence: watermark };
      markRevisionConflicts(state);
      state.syncError = null;
      state.lastSyncedAt = Date.now();
      return state;
    });
  }

  async applyRemote(object: PersonalObject | null, type: PersonalObjectType, id: string): Promise<void> {
    await this.change((state) => {
      const key = personalObjectKey(type, id);
      if (object) state.objects[key] = structuredClone(object);
      else delete state.objects[key];
      const hasDirtyDraft = state.outbox.some((item) => item.objectType === type && item.objectID === id && item.status === 'pending');
      if (hasDirtyDraft) markRevisionConflicts(state, type, id);
      return state;
    });
  }

  async markConflict(mutationID: string, message: string, remote: PersonalObject | null | undefined): Promise<void> {
    await this.change((state) => {
      const mutation = state.outbox.find((item) => item.id === mutationID);
      if (!mutation) return state;
      mutation.status = 'conflict';
      mutation.message = message;
      const key = personalObjectKey(mutation.objectType, mutation.objectID);
      if (remote) state.objects[key] = structuredClone(remote);
      else if (remote === null) delete state.objects[key];
      markRevisionConflicts(state, mutation.objectType, mutation.objectID);
      return state;
    });
  }

  async markAttachmentConflict(id: string, message: string, cancelUploadPending = false): Promise<void> {
    await this.change((state) => {
      const attachment = state.attachments.find((item) => item.id === id);
      if (attachment) { attachment.status = 'conflict'; attachment.message = message; attachment.cancelUploadPending = cancelUploadPending; }
      return state;
    });
  }

  async markAttachmentUploadCancelled(id: string): Promise<void> {
    await this.change((state) => {
      const attachment = state.attachments.find((item) => item.id === id);
      if (attachment) attachment.cancelUploadPending = false;
      return state;
    });
  }

  async rebaseAttachment(id: string, remoteRevision: number): Promise<PendingAttachment> {
    return this.change((state) => {
      const source = state.attachments.find((item) => item.id === id && item.status === 'conflict');
      if (!source) throw new Error('找不到待处理的附件草稿。');
      if (source.cancelUploadPending) throw new Error('旧上传仍待服务端取消，请先联网重试同步。');
      const next: PendingAttachment = {
        ...copyPendingAttachment(source), id: randomID(), createKey: randomID(), commitKey: randomID(),
        uploadID: undefined, baseRevision: remoteRevision, status: 'pending', message: undefined,
      };
      state.attachments.push(next);
      source.message = `${source.message ?? '上传冲突'}；保留原附件草稿，并创建了基于当前版本的重试。`;
      return state;
    }, (state) => state.attachments[state.attachments.length - 1]!);
  }

  async saveUploadID(id: string, uploadID: string): Promise<void> {
    await this.change((state) => {
      const upload = state.attachments.find((item) => item.id === id);
      if (upload) upload.uploadID = uploadID;
      return state;
    });
  }

  async completeAttachment(id: string, object: PersonalObject, cached: CachedAttachment): Promise<void> {
    await this.change((state) => {
      state.objects[personalObjectKey(object.object_type, object.object_id)] = structuredClone(object);
      state.attachments = state.attachments.filter((item) => item.id !== id);
      state.cachedAttachments = [
        ...state.cachedAttachments.filter((item) => item.attachmentID !== cached.attachmentID),
        copyCachedAttachment(cached),
      ];
      return state;
    });
  }

  async acknowledge(mutationID: string, revision: number, body: PersonalObjectBody | null): Promise<void> {
    await this.change((state) => {
      const index = state.outbox.findIndex((item) => item.id === mutationID);
      if (index < 0) return state;
      const [mutation] = state.outbox.splice(index, 1);
      if (!mutation) return state;
      const key = personalObjectKey(mutation.objectType, mutation.objectID);
      if (mutation.operation === 'delete' || !body) delete state.objects[key];
      else state.objects[key] = {
        object_type: mutation.objectType, object_id: mutation.objectID, revision, body: structuredClone(body),
      };
      for (let position = index; position < state.outbox.length; position++) {
        const later = state.outbox[position];
        if (later?.objectType === mutation.objectType && later.objectID === mutation.objectID && later.status === 'pending') {
          later.baseRevision = revision;
          break;
        }
      }
      for (const attachment of state.attachments) {
        if (attachment.dependsOnKey === mutation.key) {
          attachment.baseRevision = revision;
          attachment.dependsOnKey = undefined;
        }
      }
      state.syncError = null;
      return state;
    });
  }

  async updateCursor(afterSequence: number): Promise<void> {
    await this.change((state) => {
      state.cursor = { initialized: true, afterSequence };
      state.lastSyncedAt = Date.now();
      return state;
    });
  }

  async setSyncError(message: string | null): Promise<void> {
    await this.change((state) => { state.syncError = message; return state; });
  }

  async markSnapshotFailure(): Promise<void> {
    await this.setSyncError('无法读取个人资料快照。');
  }

  async pendingMutation(mutationID: string): Promise<PersonalMutation | undefined> {
    return (await this.read()).outbox.find((item) => item.id === mutationID);
  }

  async pendingAttachment(id: string): Promise<PendingAttachment | undefined> {
    return (await this.read()).attachments.find((item) => item.id === id);
  }

  private async change<T = void>(mutate: (state: PersonalDataState) => PersonalDataState, select?: (state: PersonalDataState) => T): Promise<T> {
    const state = await this.storage.update(this.scope, mutate);
    stateListeners.get(this.scope)?.forEach((listener) => listener(copyState(state)));
    notifySyncStatus(this.userID, this.serviceOrigin, state, this.serviceInstanceID);
    return select ? select(state) : undefined as T;
  }
}

const stateListeners = new Map<string, Set<(state: PersonalDataState) => void>>();

function markRevisionConflicts(state: PersonalDataState, type?: PersonalObjectType, id?: string): void {
  const groups = new Map<string, PersonalMutation[]>();
  for (const entry of state.outbox) {
    if (entry.status === 'conflict' && entry.resolutionQueued) continue;
    if ((entry.status !== 'pending' && entry.status !== 'conflict') || (type && entry.objectType !== type) || (id && entry.objectID !== id)) continue;
    const key = personalObjectKey(entry.objectType, entry.objectID);
    groups.set(key, [...(groups.get(key) ?? []), entry]);
  }
  for (const [key, entries] of groups) {
    let expectedRevision = state.objects[key]?.revision ?? 0;
    let blockedByConflict = false;
    for (const entry of entries.sort((left, right) => left.createdAt - right.createdAt)) {
      if (entry.status === 'conflict') {
        blockedByConflict = true;
        continue;
      }
      if (blockedByConflict || entry.baseRevision !== expectedRevision) {
        entry.status = 'conflict';
        entry.message = blockedByConflict
          ? '前序草稿存在版本冲突；此草稿已保留并暂停同步。'
          : `本地草稿基于版本 ${entry.baseRevision}，服务端或前序草稿当前版本为 ${expectedRevision}。草稿已保留。`;
        blockedByConflict = true;
      } else {
        expectedRevision += 1;
      }
    }
  }
}

function syncStatus(state: PersonalDataState): PersonalDataSyncStatus {
  return {
    pendingCount: state.outbox.filter((item) => item.status === 'pending').length + state.attachments.filter((item) => item.status === 'pending').length,
    conflictCount: state.outbox.filter((item) => item.status === 'conflict' && !item.resolutionQueued).length + state.attachments.filter((item) => item.status === 'conflict' && !item.resolutionQueued).length,
  };
}

function syncStatusUnavailable(): PersonalDataSyncStatus {
  return { pendingCount: null, conflictCount: null, error: '无法读取本地个人数据同步状态。' };
}

function notifySyncStatus(userID: string, serviceOrigin: string, state: PersonalDataState, serviceInstanceID?: string): void {
  const status = syncStatus(state);
  const scope = serviceInstanceID ? personalDataInstanceScopeKey(serviceOrigin, userID, serviceInstanceID) : personalDataScopeKey(serviceOrigin, userID);
  statusListeners.get(scope)?.forEach((listener) => listener(status));
}

function randomID(): string {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function isConflict(error: unknown): boolean {
  const value = error as { status?: number; code?: string } | null;
  return value?.status === 409 || value?.code === 'conflict' || value?.code === 'revision_conflict';
}

function isNotFound(error: unknown): boolean {
  const value = error as { status?: number; code?: string } | null;
  return value?.status === 404 || value?.code === 'not_found';
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : '同步失败，请稍后重试。';
}

function isOnline(): boolean {
  return typeof navigator === 'undefined' || navigator.onLine !== false;
}

function syncIsActive(shouldContinue: () => boolean): boolean {
  return shouldContinue() && isOnline();
}

/** Pulls a complete remote snapshot/change page, then retries local mutations and attachments. */
export async function synchronizePersonalData(
  store: PersonalDataStore,
  api: PersonalDataAPI,
  shouldContinue: () => boolean = () => true,
): Promise<void> {
  if (!shouldContinue()) return;
  if (!isOnline()) {
    await store.setSyncError('当前离线；本地数据和待同步草稿已保留。');
    return;
  }
  if (!store.serviceInstanceID) {
    await store.setSyncError('当前服务实例尚未确认；本地草稿已保留，确认后再自动提交。');
    return;
  }
  const locks = typeof navigator === 'undefined' ? undefined : navigator.locks;
  if (!locks) {
    await store.setSyncError('当前浏览器不支持跨标签页安全同步；本地草稿已保留。');
    return;
  }
  const lockName = `morde:personal-data-drain:v1:${encodeURIComponent(store.scope)}`;
  try {
    await locks.request(lockName, { mode: 'exclusive' }, async () => {
      if (shouldContinue()) await synchronizePersonalDataWithLock(store, api, shouldContinue);
    });
  } catch (error) {
    if (shouldContinue()) await store.setSyncError(messageOf(error));
  }
}

async function synchronizePersonalDataWithLock(
  store: PersonalDataStore,
  api: PersonalDataAPI,
  shouldContinue: () => boolean,
): Promise<void> {
  try {
    const initial = await store.read();
    if (!syncIsActive(shouldContinue)) return;
    if (!initial.snapshotComplete) {
      const objects: PersonalObject[] = [];
      let cursor: string | undefined;
      let watermark = 0;
      do {
        if (!syncIsActive(shouldContinue)) return;
        const page = await api.personalDataSnapshot(cursor, 100);
        if (!syncIsActive(shouldContinue)) return;
        watermark = page.watermark;
        objects.push(...page.items);
        cursor = page.next_cursor ?? undefined;
      } while (cursor);
      if (!syncIsActive(shouldContinue)) return;
      await store.applySnapshot(objects, watermark);
    } else {
      let current = await store.read();
      let after = current.cursor.afterSequence;
      let through: number | undefined;
      let hasMore = true;
      while (hasMore) {
        if (!syncIsActive(shouldContinue)) return;
        const page = await api.personalDataChanges(after, through, 100);
        if (!syncIsActive(shouldContinue)) return;
        through = page.through_sequence;
        for (const change of page.items) {
          if (!syncIsActive(shouldContinue)) return;
          const currentState = await store.read();
          if (!syncIsActive(shouldContinue)) return;
          const matching = currentState.outbox.find((entry) => entry.key === change.mutation_id);
          if (matching) {
            if (change.operation === 'delete') {
              await store.acknowledge(matching.id, change.revision, null);
            } else {
              const object = await api.getPersonalObject(change.object_type, change.object_id);
              if (!syncIsActive(shouldContinue)) return;
              await store.acknowledge(matching.id, object.revision, object.body);
            }
          } else if (change.operation === 'delete') {
            await store.applyRemote(null, change.object_type, change.object_id);
          } else {
            const object = await api.getPersonalObject(change.object_type, change.object_id);
            if (!syncIsActive(shouldContinue)) return;
            await store.applyRemote(object, change.object_type, change.object_id);
          }
        }
        if (!syncIsActive(shouldContinue)) return;
        after = page.next_after_sequence;
        hasMore = page.has_more;
        await store.updateCursor(after);
        current = await store.read();
      }
      if (!current.cursor.initialized) await store.updateCursor(after);
    }

    let state = await store.read();
    if (!syncIsActive(shouldContinue)) return;
    const pending = state.outbox.filter((item) => item.status === 'pending').sort((left, right) => left.createdAt - right.createdAt);
    for (const entry of pending) {
      try {
        if (!syncIsActive(shouldContinue)) return;
        const current = await store.pendingMutation(entry.id);
        if (!syncIsActive(shouldContinue)) return;
        if (!current || current.status !== 'pending') continue;
        let receipt: { result_revision: number; account_change_sequence: number };
        if (current.operation === 'delete') {
          if (!syncIsActive(shouldContinue)) return;
          receipt = await api.deletePersonalObject(current.objectType, current.objectID, current.baseRevision, current.key);
          await store.acknowledge(current.id, receipt.result_revision, null);
        } else if (current.body) {
          if (!syncIsActive(shouldContinue)) return;
          receipt = await api.replacePersonalObject(current.objectType, current.objectID, current.baseRevision, current.body, current.key);
          await store.acknowledge(current.id, receipt.result_revision, current.body);
        }
      } catch (error) {
        if (!shouldContinue()) return;
        if (isConflict(error)) {
          let remote: PersonalObject | null | undefined;
          try { remote = await api.getPersonalObject(entry.objectType, entry.objectID); }
          catch (readError) { remote = isNotFound(readError) ? null : undefined; }
          await store.markConflict(entry.id, messageOf(error), remote);
          continue;
        }
        await store.setSyncError(messageOf(error));
        return;
      }
    }

    state = await store.read();
    if (!syncIsActive(shouldContinue)) return;
    for (const attachment of state.attachments.filter((item) => item.status === 'conflict' && item.cancelUploadPending && item.uploadID)) {
      if (!syncIsActive(shouldContinue)) return;
      try {
        await api.cancelPersonalAttachmentUpload(attachment.uploadID!);
        if (!shouldContinue()) return;
        await store.markAttachmentUploadCancelled(attachment.id);
      } catch (error) {
        if (!shouldContinue()) return;
        if (isNotFound(error)) {
          await store.markAttachmentUploadCancelled(attachment.id);
          continue;
        }
        await store.setSyncError(messageOf(error));
        return;
      }
    }
    state = await store.read();
    if (!syncIsActive(shouldContinue)) return;
    for (const attachment of state.attachments.filter((item) => item.status === 'pending')) {
      try {
        if (!syncIsActive(shouldContinue)) return;
        const current = await store.pendingAttachment(attachment.id);
        if (!syncIsActive(shouldContinue)) return;
        if (!current || current.status !== 'pending' || current.dependsOnKey) continue;
        let uploadID = current.uploadID;
        if (!uploadID) {
          if (!syncIsActive(shouldContinue)) return;
          const receipt = await api.createPersonalAttachmentUpload({
            object_id: current.objectID, base_revision: current.baseRevision,
            size_bytes: current.sizeBytes, sha256: current.sha256, media_type: current.mediaType,
          }, current.createKey);
          uploadID = receipt.upload_id;
          await store.saveUploadID(current.id, uploadID);
          // Persist the remote upload handle even when the user changed service scope while
          // the create request was in flight. A later sync of the original scope can resume
          // or cancel this upload without leaking an orphan on the old service.
          if (!shouldContinue()) return;
        }
        const chunkBytes = 4 * 1024 * 1024;
        for (let offset = 0, sequence = 0; offset < current.sizeBytes; offset += chunkBytes, sequence++) {
          if (!syncIsActive(shouldContinue)) return;
          const bytes = await current.blob.slice(offset, Math.min(offset + chunkBytes, current.sizeBytes)).arrayBuffer();
          const chunkDigest = await sha256Hex(bytes);
          if (!syncIsActive(shouldContinue)) return;
          await api.putPersonalAttachmentChunk(uploadID, sequence, bytes, chunkDigest);
        }
        if (!syncIsActive(shouldContinue)) return;
        const receipt = await api.commitPersonalAttachment(uploadID, current.commitKey);
        if (!shouldContinue()) return;
        let object: PersonalObject;
        try {
          object = await api.getPersonalObject('library_item', current.objectID);
          if (!shouldContinue()) return;
        }
        catch {
          if (!shouldContinue()) return;
          object = {
            object_type: 'library_item', object_id: current.objectID, revision: receipt.result_revision,
            body: { name: current.name, kind: current.kind, mime: current.mediaType, size_bytes: current.sizeBytes, attachment_id: receipt.attachment_id },
          };
        }
        await store.completeAttachment(current.id, object, {
          attachmentID: receipt.attachment_id, name: current.name, mediaType: current.mediaType, blob: current.blob,
        });
      } catch (error) {
        if (!shouldContinue()) return;
        if (isConflict(error)) {
          const cancelPending = Boolean(attachment.uploadID);
          try {
            const remote = await api.getPersonalObject('library_item', attachment.objectID);
            await store.applyRemote(remote, 'library_item', attachment.objectID);
          } catch (readError) {
            if (isNotFound(readError)) await store.applyRemote(null, 'library_item', attachment.objectID);
          }
          await store.markAttachmentConflict(attachment.id, messageOf(error), cancelPending);
          continue;
        }
        await store.setSyncError(messageOf(error));
        return;
      }
    }
    if (shouldContinue()) await store.setSyncError(null);
  } catch (error) {
    if (shouldContinue()) await store.setSyncError(messageOf(error));
  }
}

export async function sha256Hex(value: ArrayBuffer | Uint8Array): Promise<string> {
  const bytes = value instanceof Uint8Array ? value : new Uint8Array(value);
  const stableBytes = new Uint8Array(bytes.byteLength);
  stableBytes.set(bytes);
  const digest = await globalThis.crypto.subtle.digest('SHA-256', stableBytes.buffer);
  return [...new Uint8Array(digest)].map((part) => part.toString(16).padStart(2, '0')).join('');
}
