import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createMemoryPersonalDataStorage,
  PersonalDataStore,
  personalDataLegacyScopeKey,
  personalDataScopeKey,
  sha256Hex,
  subscribePersonalDataSyncStatus,
  synchronizePersonalData,
  type PersonalDataAPI,
  type PersonalDataSyncStatus,
  type PersonalObject,
} from './personal-data-store';

function api(overrides: Partial<PersonalDataAPI> = {}): PersonalDataAPI {
  return {
    personalDataSnapshot: vi.fn().mockResolvedValue({ watermark: 0, items: [], next_cursor: null }),
    personalDataChanges: vi.fn().mockImplementation(async (after: number, through?: number) => ({
      items: [], through_sequence: through ?? after, next_after_sequence: through ?? after, has_more: false,
    })),
    getPersonalObject: vi.fn(),
    replacePersonalObject: vi.fn().mockResolvedValue({ result_revision: 1, account_change_sequence: 1 }),
    deletePersonalObject: vi.fn().mockResolvedValue({ result_revision: 1, account_change_sequence: 1 }),
    personalDataQuota: vi.fn().mockResolvedValue({ used_bytes: 0, reserved_bytes: 0, quota_bytes: 1000, remaining_bytes: 1000, max_attachment_bytes: 1000 }),
    createPersonalAttachmentUpload: vi.fn(),
    putPersonalAttachmentChunk: vi.fn(),
    commitPersonalAttachment: vi.fn(),
    cancelPersonalAttachmentUpload: vi.fn(),
    personalAttachment: vi.fn(),
    ...overrides,
  };
}

function makeLocks(): LockManager {
  const tails = new Map<string, Promise<void>>();
  return {
    request: async <T>(name: string, _options: LockOptions, callback: (lock: Lock | null) => T | PromiseLike<T>): Promise<T> => {
      const previous = tails.get(name) ?? Promise.resolve();
      let release!: () => void;
      const current = new Promise<void>((resolve) => { release = resolve; });
      const tail = previous.then(() => current);
      tails.set(name, tail);
      await previous;
      try { return await callback({ name, mode: 'exclusive' } as Lock); }
      finally {
        release();
        if (tails.get(name) === tail) tails.delete(name);
      }
    },
  } as unknown as LockManager;
}

function makeStore(userID: string, origin = 'https://portal.example.test', storage = createMemoryPersonalDataStorage(), serviceInstanceID = 'service-test') {
  return new PersonalDataStore(userID, origin, storage, serviceInstanceID);
}

function profile(revision: number, name: string): PersonalObject {
  return {
    object_type: 'profile', object_id: 'default', revision,
    body: { name, avatar: 'dragon', emoji: '', color: '', description: '', style: '' },
  };
}

const originalLocks = Object.getOwnPropertyDescriptor(navigator, 'locks');

beforeEach(() => {
  Object.defineProperty(navigator, 'locks', { configurable: true, value: makeLocks() });
});

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
  if (originalLocks) Object.defineProperty(navigator, 'locks', originalLocks);
  else Reflect.deleteProperty(navigator, 'locks');
});

describe('PersonalDataStore', () => {
  it('isolates persistent state by service origin and user ID', async () => {
    const storage = createMemoryPersonalDataStorage();
    const storeA = new PersonalDataStore('u_alice', 'https://one.example.test/path', storage, 'instance-a');
    const storeB = new PersonalDataStore('u_alice', 'https://two.example.test', storage, 'instance-a');
    const storeC = new PersonalDataStore('u_bob', 'https://one.example.test', storage, 'instance-a');
    await storeA.queueReplace('memory', 'global', { content: 'local note' });

    expect(personalDataScopeKey('https://one.example.test/path', 'u_alice')).not.toBe(personalDataScopeKey('https://two.example.test', 'u_alice'));
    expect((await storeA.read()).outbox).toHaveLength(1);
    expect((await storeB.read()).outbox).toHaveLength(0);
    expect((await storeC.read()).outbox).toHaveLength(0);
  });

  it('isolates one origin and user when the service instance changes and quarantines the old drafts', async () => {
    const storage = createMemoryPersonalDataStorage();
    const old = new PersonalDataStore('u_instance_change', 'https://portal.example.test', storage, 'instance-old');
    const current = new PersonalDataStore('u_instance_change', 'https://portal.example.test', storage, 'instance-new');
    await old.applySnapshot([profile(7, 'Old confirmed profile')], 21);
    const draft = await old.queueReplace('profile', 'default', { name: 'Old unsent draft', avatar: 'dragon' });
    const blob = new Blob(['kept bytes'], { type: 'text/plain' });
    await old.queueLibraryAttachment({
      objectID: 'library-old', name: 'kept.txt', kind: 'artifact', mediaType: blob.type,
      sizeBytes: blob.size, sha256: 'b'.repeat(64), blob,
    }, { name: 'kept.txt', kind: 'artifact', mime: blob.type, size_bytes: blob.size });

    expect((await current.read()).snapshotComplete).toBe(false);
    expect((await current.read()).outbox).toHaveLength(0);
    await current.quarantineOtherInstanceDrafts();
    const oldState = await old.read();
    const candidates = await current.otherInstanceDraftScopes();
    expect(candidates).toHaveLength(1);
    expect(candidates[0]?.serviceInstanceID).toBe('instance-old');
    expect(oldState.cursor.afterSequence).toBe(21);
    expect(oldState.outbox[0]).toMatchObject({ id: draft.id, key: draft.key, status: 'conflict', body: { name: 'Old unsent draft' } });
    expect(oldState.attachments[0]).toMatchObject({ status: 'conflict', blob, createKey: expect.any(String), commitKey: expect.any(String) });
  });

  it('finds and preserves drafts stored under the pre-instance scope', async () => {
    const storage = createMemoryPersonalDataStorage();
    const origin = 'https://portal.example.test';
    const oldScope = personalDataLegacyScopeKey(origin, 'u_legacy_scope');
    await storage.update(oldScope, (state) => {
      state.outbox.push({ id: 'legacy-mutation', key: 'legacy-idempotency-key', objectType: 'profile', objectID: 'default',
        baseRevision: 3, body: { name: 'Legacy draft' }, operation: 'replace', status: 'pending', createdAt: 1 });
      return state;
    });
    const current = new PersonalDataStore('u_legacy_scope', origin, storage, 'instance-current');

    await current.quarantineOtherInstanceDrafts();

    const candidates = await current.otherInstanceDraftScopes();
    expect(candidates).toHaveLength(1);
    expect(candidates[0]).toMatchObject({ scope: oldScope, serviceInstanceID: null });
    expect(candidates[0]?.state.outbox[0]).toMatchObject({ id: 'legacy-mutation', key: 'legacy-idempotency-key', status: 'conflict', body: { name: 'Legacy draft' } });
  });

  it('serializes drains from two tabs that share the same account and service-instance lock', async () => {
    const storage = createMemoryPersonalDataStorage();
    const firstTab = new PersonalDataStore('u_two_tabs', 'https://portal.example.test', storage, 'instance-shared');
    const secondTab = new PersonalDataStore('u_two_tabs', 'https://portal.example.test', storage, 'instance-shared');
    let releaseSnapshot!: (value: { watermark: number; items: PersonalObject[]; next_cursor: null }) => void;
    const snapshotBlocked = new Promise<{ watermark: number; items: PersonalObject[]; next_cursor: null }>((resolve) => { releaseSnapshot = resolve; });
    let activeCalls = 0;
    let maxActiveCalls = 0;
    const enter = () => { activeCalls += 1; maxActiveCalls = Math.max(maxActiveCalls, activeCalls); };
    const leave = () => { activeCalls -= 1; };
    const firstAPI = api({
      personalDataSnapshot: vi.fn(async () => { enter(); try { return await snapshotBlocked; } finally { leave(); } }),
      personalDataChanges: vi.fn(async (after) => { enter(); try { return { items: [], through_sequence: after, next_after_sequence: after, has_more: false }; } finally { leave(); } }),
    });
    const secondAPI = api({
      personalDataSnapshot: vi.fn(async () => { enter(); try { return { watermark: 0, items: [], next_cursor: null }; } finally { leave(); } }),
      personalDataChanges: vi.fn(async (after) => { enter(); try { return { items: [], through_sequence: after, next_after_sequence: after, has_more: false }; } finally { leave(); } }),
    });
    const firstDrain = synchronizePersonalData(firstTab, firstAPI);
    await vi.waitFor(() => expect(firstAPI.personalDataSnapshot).toHaveBeenCalledTimes(1));
    const secondDrain = synchronizePersonalData(secondTab, secondAPI);
    await Promise.resolve();
    expect(secondAPI.personalDataSnapshot).not.toHaveBeenCalled();
    expect(secondAPI.personalDataChanges).not.toHaveBeenCalled();
    releaseSnapshot({ watermark: 0, items: [], next_cursor: null });
    await Promise.all([firstDrain, secondDrain]);

    expect(maxActiveCalls).toBe(1);
    expect(firstAPI.personalDataSnapshot).toHaveBeenCalledTimes(1);
    expect(secondAPI.personalDataSnapshot).not.toHaveBeenCalled();
    expect(secondAPI.personalDataChanges).toHaveBeenCalledTimes(1);
  });

  it('fails closed without Web Locks and keeps the pending draft unchanged', async () => {
    const store = makeStore('u_no_web_locks');
    const pending = await store.queueReplace('profile', 'default', { name: 'Do not lose me', avatar: 'dragon' });
    Object.defineProperty(navigator, 'locks', { configurable: true, value: undefined });
    const activeAPI = api();
    await synchronizePersonalData(store, activeAPI);

    const state = await store.read();
    expect(state.outbox).toHaveLength(1);
    expect(state.outbox[0]).toMatchObject({ id: pending.id, key: pending.key, status: 'pending', body: { name: 'Do not lose me' } });
    expect(state.syncError).toMatch(/不支持跨标签页安全同步/);
    expect(activeAPI.personalDataSnapshot).not.toHaveBeenCalled();
    expect(activeAPI.replacePersonalObject).not.toHaveBeenCalled();
  });

  it('copies an old draft to the confirmed current version only after explicit selection and retains original blobs and keys', async () => {
    const storage = createMemoryPersonalDataStorage();
    const old = new PersonalDataStore('u_import_old', 'https://portal.example.test', storage, 'instance-old');
    const current = new PersonalDataStore('u_import_old', 'https://portal.example.test', storage, 'instance-new');
    await old.queueLibraryAttachment({
      objectID: 'library-entry', name: 'old.txt', kind: 'artifact', mediaType: 'text/plain',
      sizeBytes: 8, sha256: 'c'.repeat(64), blob: new Blob(['old file']),
    }, { name: 'old.txt', kind: 'artifact', mime: 'text/plain', size_bytes: 8 });
    const oldStateBefore = await old.read();
    const oldMutation = oldStateBefore.outbox[0]!;
    const oldAttachment = oldStateBefore.attachments[0]!;
    await old.applySnapshot([profile(2, 'Old remote')], 2);
    await current.applySnapshot([profile(5, 'New service remote')], 12);
    await current.quarantineOtherInstanceDrafts();

    const importedMutation = await current.importOtherInstanceMutation(old.scope, oldMutation.id);
    expect(importedMutation.baseRevision).toBe(0);
    expect(importedMutation.body?.name).toBe('old.txt');
    expect(importedMutation.key).not.toBe(oldMutation.key);
    expect(await current.importOtherInstanceMutation(old.scope, oldMutation.id)).toEqual(importedMutation);
    const importedAttachment = await current.importOtherInstanceAttachment(old.scope, oldAttachment.id);
    expect(importedAttachment).toMatchObject({ status: 'pending', dependsOnKey: importedMutation.key, uploadID: undefined, blob: oldAttachment.blob });
    expect(importedAttachment.createKey).not.toBe(oldAttachment.createKey);
    expect(importedAttachment.commitKey).not.toBe(oldAttachment.commitKey);
    expect(await current.importOtherInstanceAttachment(old.scope, oldAttachment.id)).toEqual(importedAttachment);

    const currentState = await current.read();
    const oldState = await old.read();
    expect(currentState.outbox.filter((item) => item.migrationSource?.id === oldMutation.id)).toHaveLength(1);
    expect(currentState.attachments.filter((item) => item.migrationSource?.id === oldAttachment.id)).toHaveLength(1);
    expect(oldState.outbox[0]).toMatchObject({ id: oldMutation.id, key: oldMutation.key, status: 'conflict', resolutionQueued: true });
    expect(oldState.attachments[0]).toMatchObject({ id: oldAttachment.id, createKey: oldAttachment.createKey, commitKey: oldAttachment.commitKey, status: 'conflict', resolutionQueued: true });
  });

  it('reports an explicit unknown status when IndexedDB is unavailable', async () => {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'indexedDB');
    Object.defineProperty(globalThis, 'indexedDB', { configurable: true, value: undefined });
    try {
      const status = await new Promise<PersonalDataSyncStatus>((resolve) => {
        let unsubscribe: () => void = () => undefined;
        unsubscribe = subscribePersonalDataSyncStatus('u_no_storage', (next) => {
          if (next.error) { unsubscribe(); resolve(next); }
        }, 'https://portal.example.test');
      });
      expect(status).toEqual({ pendingCount: null, conflictCount: null, error: '无法读取本地个人数据同步状态。' });
    } finally {
      if (descriptor) Object.defineProperty(globalThis, 'indexedDB', descriptor);
      else Reflect.deleteProperty(globalThis, 'indexedDB');
    }
  });

  it('ignores the legacy pause setting and retries the exact same idempotency key', async () => {
    const userID = 'u_legacy_pause';
    const store = makeStore(userID);
    await store.queueReplace('profile', 'default', { name: 'Draft', avatar: 'dragon' });
    const legacySettingKey = `morde.personal-data.sync.v1:${encodeURIComponent(new URL(store.serviceOrigin).origin)}:${encodeURIComponent(userID)}`;
    localStorage.setItem(legacySettingKey, 'false');
    const replace = vi.fn()
      .mockRejectedValueOnce(new Error('network disconnected'))
      .mockResolvedValueOnce({ result_revision: 1, account_change_sequence: 1 });
    const activeAPI = api({ replacePersonalObject: replace });
    await synchronizePersonalData(store, activeAPI);
    expect(activeAPI.personalDataSnapshot).toHaveBeenCalledTimes(1);
    const savedKey = replace.mock.calls[0]?.[4];
    expect(savedKey).toBeTruthy();
    expect((await store.read()).outbox).toHaveLength(1);

    await synchronizePersonalData(store, activeAPI);
    expect(replace).toHaveBeenCalledTimes(2);
    expect(replace.mock.calls[1]?.[4]).toBe(savedKey);
    expect((await store.read()).outbox).toHaveLength(0);
    expect((await store.read()).objects['["profile","default"]']?.body.name).toBe('Draft');
  });

  it('preserves a revision-conflicted draft and requires an explicit rebase', async () => {
    const store = makeStore('u_conflict');
    await store.applySnapshot([profile(3, 'Remote original')], 3);
    const draft = await store.queueReplace('profile', 'default', { name: 'My draft', avatar: 'dragon' });
    const laterDraft = await store.queueReplace('profile', 'default', { name: 'My later draft', avatar: 'dragon' });
    const apiForConflict = api({
      personalDataChanges: vi.fn().mockResolvedValue({ items: [], through_sequence: 3, next_after_sequence: 3, has_more: false }),
      replacePersonalObject: vi.fn().mockRejectedValue(Object.assign(new Error('revision conflict'), { status: 409 })),
      getPersonalObject: vi.fn().mockResolvedValue(profile(4, 'Remote changed elsewhere')),
    });

    await synchronizePersonalData(store, apiForConflict);
    const state = await store.read();
    expect(state.outbox).toHaveLength(2);
    expect(state.outbox[0]?.status).toBe('conflict');
    expect(state.outbox[0]?.key).toBe(draft.key);
    expect(state.outbox[0]?.body?.name).toBe('My draft');
    expect(state.outbox[1]?.status).toBe('conflict');
    expect(state.outbox[1]?.key).toBe(laterDraft.key);
    expect(state.outbox[1]?.body?.name).toBe('My later draft');
    expect(state.objects['["profile","default"]']?.body.name).toBe('Remote changed elsewhere');
    expect((await store.visibleObjects()).find((item) => item.object_type === 'profile')?.body.name).toBe('My later draft');
    await expect(store.queueReplace('profile', 'default', { name: 'Accidental overwrite', avatar: 'dragon' })).rejects.toThrow(/冲突/);

    const rebased = await store.rebaseDraft(draft.id, { name: 'My reviewed draft', avatar: 'dragon' });
    const afterRebase = await store.read();
    expect(rebased.key).not.toBe(draft.key);
    expect(rebased.baseRevision).toBe(4);
    expect(afterRebase.outbox).toHaveLength(3);
    expect(afterRebase.outbox[0]?.body?.name).toBe('My draft');
    expect(afterRebase.outbox[0]?.resolutionQueued).toBe(true);
    expect(afterRebase.outbox[1]?.body?.name).toBe('My later draft');
    expect(afterRebase.outbox[1]?.status).toBe('conflict');
    expect(afterRebase.outbox[2]?.body?.name).toBe('My reviewed draft');
  });

  it('persists pending attachment bytes and stable create/commit keys in the local outbox', async () => {
    const store = makeStore('u_attachment');
    const blob = new Blob(['draft bytes'], { type: 'text/plain' });
    await store.queueLibraryAttachment({
      objectID: 'library-entry', name: 'draft.txt', kind: 'artifact', mediaType: 'text/plain',
      sizeBytes: blob.size, sha256: 'a'.repeat(64), blob,
    }, { name: 'draft.txt', kind: 'artifact', mime: 'text/plain', size_bytes: blob.size });
    const stored = await store.read();
    const attachment = stored.attachments[0]!;
    expect(stored.outbox[0]?.body?.name).toBe('draft.txt');
    expect(attachment.blob.size).toBe(blob.size);
    expect(attachment.dependsOnKey).toBe(stored.outbox[0]?.key);
    expect(attachment.createKey).toBeTruthy();
    expect(attachment.commitKey).toBeTruthy();
    expect((await store.pendingAttachment(attachment.id))?.uploadID).toBeUndefined();
  });

  it('uploads an attachment in bounded hashed chunks and commits with its persisted keys', async () => {
    const store = makeStore('u_upload');
    const original: PersonalObject = {
      object_type: 'library_item', object_id: 'library-entry', revision: 1,
      body: { name: 'draft.bin', kind: 'artifact', mime: 'application/octet-stream', size_bytes: 0 },
    };
    await store.applySnapshot([original], 1);
    const blob = new Blob([new Uint8Array((4 * 1024 * 1024) + 3)], { type: 'application/octet-stream' });
    const job = await store.queueAttachment({
      objectID: original.object_id, name: 'draft.bin', kind: 'artifact', mediaType: blob.type,
      sizeBytes: blob.size, sha256: await sha256Hex(await blob.arrayBuffer()), blob, baseRevision: 1,
    });
    const upload = vi.fn().mockResolvedValue({ upload_id: 'pu_upload-session', state: 'active' });
    const putChunk = vi.fn().mockResolvedValue({});
    const commit = vi.fn().mockResolvedValue({ attachment_id: `pa_${'a'.repeat(32)}`, result_revision: 2, object_id: original.object_id });
    const attached: PersonalObject = {
      ...original, revision: 2,
      body: { ...original.body, size_bytes: blob.size, attachment_id: `pa_${'a'.repeat(32)}` },
    };
    const getObject = vi.fn().mockResolvedValue(attached);
    const uploadAPI = api({ createPersonalAttachmentUpload: upload, putPersonalAttachmentChunk: putChunk, commitPersonalAttachment: commit, getPersonalObject: getObject });

    await synchronizePersonalData(store, uploadAPI);

    expect(upload).toHaveBeenCalledWith({
      object_id: original.object_id, base_revision: 1, size_bytes: blob.size,
      sha256: job.sha256, media_type: blob.type,
    }, job.createKey);
    expect(putChunk).toHaveBeenCalledTimes(2);
    expect((putChunk.mock.calls[0]?.[2] as ArrayBuffer).byteLength).toBe(4 * 1024 * 1024);
    expect((putChunk.mock.calls[1]?.[2] as ArrayBuffer).byteLength).toBe(3);
    expect(putChunk.mock.calls.every((call) => typeof call[3] === 'string' && call[3].length === 64)).toBe(true);
    expect(commit).toHaveBeenCalledWith('pu_upload-session', job.commitKey);
    expect((await store.read()).attachments).toHaveLength(0);
    expect((await store.read()).objects['["library_item","library-entry"]']?.revision).toBe(2);
    expect((await store.cachedAttachment(`pa_${'a'.repeat(32)}`))?.blob.size).toBe(blob.size);
  });
});
