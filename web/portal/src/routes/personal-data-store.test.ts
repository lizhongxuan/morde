import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  createMemoryPersonalDataStorage,
  getPersonalDataSyncEnabled,
  getPersonalDataSyncStatus,
  PersonalDataStore,
  personalDataScopeKey,
  personalDataSyncSettingKey,
  setPersonalDataSyncEnabled,
  sha256Hex,
  subscribePersonalDataSyncEnabled,
  subscribePersonalDataSyncStatus,
  synchronizePersonalData,
  type PersonalDataAPI,
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

function makeStore(userID: string, origin = 'https://portal.example.test') {
  return new PersonalDataStore(userID, origin, createMemoryPersonalDataStorage());
}

function profile(revision: number, name: string): PersonalObject {
  return {
    object_type: 'profile', object_id: 'default', revision,
    body: { name, avatar: 'dragon', emoji: '', color: '', description: '', style: '' },
  };
}

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
});

describe('PersonalDataStore', () => {
  it('isolates persistent state and sync settings by service origin and user ID', async () => {
    const storage = createMemoryPersonalDataStorage();
    const storeA = new PersonalDataStore('u_alice', 'https://one.example.test/path', storage);
    const storeB = new PersonalDataStore('u_alice', 'https://two.example.test', storage);
    const storeC = new PersonalDataStore('u_bob', 'https://one.example.test', storage);
    await storeA.queueReplace('memory', 'global', { content: 'local note' });

    expect(personalDataScopeKey('https://one.example.test/path', 'u_alice')).not.toBe(personalDataScopeKey('https://two.example.test', 'u_alice'));
    expect((await storeA.read()).outbox).toHaveLength(1);
    expect((await storeB.read()).outbox).toHaveLength(0);
    expect((await storeC.read()).outbox).toHaveLength(0);

    expect(getPersonalDataSyncEnabled('u_alice', 'https://one.example.test')).toBe(true);
    expect(personalDataSyncSettingKey('u_alice', 'https://one.example.test')).not.toBe(personalDataSyncSettingKey('u_alice', 'https://two.example.test'));
    setPersonalDataSyncEnabled('u_alice', false, 'https://one.example.test');
    expect(getPersonalDataSyncEnabled('u_alice', 'https://one.example.test')).toBe(false);
    expect(getPersonalDataSyncEnabled('u_alice', 'https://two.example.test')).toBe(true);
    expect(getPersonalDataSyncEnabled('u_bob', 'https://one.example.test')).toBe(true);
  });

  it('notifies same-tab sync-setting subscribers', () => {
    const listener = vi.fn();
    const unsubscribe = subscribePersonalDataSyncEnabled('u_subscriber', listener, 'https://portal.example.test');
    setPersonalDataSyncEnabled('u_subscriber', false, 'https://portal.example.test');
    setPersonalDataSyncEnabled('u_subscriber', true, 'https://portal.example.test');
    unsubscribe();
    setPersonalDataSyncEnabled('u_subscriber', false, 'https://portal.example.test');
    expect(listener.mock.calls.map(([enabled]) => enabled)).toEqual([false, true]);
  });

  it('reports an explicit unknown status when IndexedDB is unavailable', async () => {
    const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'indexedDB');
    Object.defineProperty(globalThis, 'indexedDB', { configurable: true, value: undefined });
    try {
      const status = await new Promise<Awaited<ReturnType<typeof getPersonalDataSyncStatus>>>((resolve) => {
        let unsubscribe: () => void = () => undefined;
        unsubscribe = subscribePersonalDataSyncStatus('u_no_storage', (next) => {
          if (next.error) { unsubscribe(); resolve(next); }
        }, 'https://portal.example.test');
      });
      expect(status).toEqual({ enabled: true, pendingCount: null, conflictCount: null, error: '无法读取本地个人数据同步状态。' });
    } finally {
      if (descriptor) Object.defineProperty(globalThis, 'indexedDB', descriptor);
      else Reflect.deleteProperty(globalThis, 'indexedDB');
    }
  });

  it('keeps local drafts while sync is disabled and retries the exact same idempotency key', async () => {
    const store = makeStore('u_offline');
    await store.queueReplace('profile', 'default', { name: 'Draft', avatar: 'dragon' });
    setPersonalDataSyncEnabled('u_offline', false, store.serviceOrigin);
    const pausedAPI = api();

    await synchronizePersonalData(store, pausedAPI);
    expect(pausedAPI.personalDataSnapshot).not.toHaveBeenCalled();
    expect((await store.read()).outbox).toHaveLength(1);

    setPersonalDataSyncEnabled('u_offline', true, store.serviceOrigin);
    const replace = vi.fn()
      .mockRejectedValueOnce(new Error('network disconnected'))
      .mockResolvedValueOnce({ result_revision: 1, account_change_sequence: 1 });
    const activeAPI = api({ replacePersonalObject: replace });
    await synchronizePersonalData(store, activeAPI);
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
