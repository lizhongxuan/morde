import { describe, expect, it, vi } from 'vitest';
import { ResilientAssistantDraftStorage } from './assistant-drafts';
import type { AssistantComposerDraft, AssistantDraftStorage } from './assistant-drafts';

describe('assistant draft recovery', () => {
  it('keeps a newer in-memory draft when a durable write fails', async () => {
    const old: AssistantComposerDraft = { id: 'c1234567', conversationID: 'c1234567', content: '旧草稿',
      modelID: '', hostID: '', mcpConnectorIDs: [], attachments: [], updatedAt: 1 };
    const updated = { ...old, content: '最新输入', updatedAt: 2 };
    const durable: AssistantDraftStorage = {
      get: vi.fn().mockResolvedValue(old), list: vi.fn().mockResolvedValue([old]),
      put: vi.fn().mockRejectedValue(new Error('storage full')), remove: vi.fn(),
    };
    const storage = new ResilientAssistantDraftStorage(durable);
    await expect(storage.put('scope', updated)).rejects.toThrow('storage full');
    expect(await storage.get('scope', old.id)).toEqual(updated);
    expect(await storage.list('scope')).toEqual([updated]);
  });

  it('does not replace a newer local write with an older read already in flight', async () => {
    const old: AssistantComposerDraft = { id: 'c1234567', conversationID: 'c1234567', content: '旧草稿',
      modelID: '', hostID: '', mcpConnectorIDs: [], attachments: [], updatedAt: 1 };
    const updated = { ...old, content: '最新输入', updatedAt: 2 };
    let finishRead!: (draft: AssistantComposerDraft | null) => void;
    const durable: AssistantDraftStorage = {
      get: vi.fn().mockImplementation(() => new Promise((resolve) => { finishRead = resolve; })),
      list: vi.fn().mockResolvedValue([]), put: vi.fn().mockResolvedValue(undefined), remove: vi.fn(),
    };
    const storage = new ResilientAssistantDraftStorage(durable);
    await storage.put('scope', old);
    const reading = storage.get('scope', old.id);
    await storage.put('scope', updated);
    finishRead(old);
    expect(await reading).toEqual(updated);
  });

  it('does not resurrect a removed draft from reads that were already in flight', async () => {
    const old: AssistantComposerDraft = { id: 'local:one', conversationID: null, content: '旧草稿',
      modelID: '', hostID: '', mcpConnectorIDs: [], attachments: [], updatedAt: 1 };
    let finishGet!: (draft: AssistantComposerDraft | null) => void;
    let finishList!: (drafts: AssistantComposerDraft[]) => void;
    const durable: AssistantDraftStorage = {
      get: vi.fn().mockImplementation(() => new Promise((resolve) => { finishGet = resolve; })),
      list: vi.fn().mockImplementation(() => new Promise((resolve) => { finishList = resolve; })),
      put: vi.fn().mockResolvedValue(undefined), remove: vi.fn().mockResolvedValue(undefined),
    };
    const storage = new ResilientAssistantDraftStorage(durable);
    const reading = storage.get('scope', old.id);
    const listing = storage.list('scope');
    await storage.remove('scope', old.id);
    finishGet(old);
    finishList([old]);
    expect(await reading).toBeNull();
    expect(await listing).toEqual([]);
  });

  it('preserves a new draft written after a pending removal', async () => {
    const old: AssistantComposerDraft = { id: 'local:one', conversationID: null, content: '旧草稿',
      modelID: '', hostID: '', mcpConnectorIDs: [], attachments: [], updatedAt: 1 };
    const updated = { ...old, content: '新草稿', updatedAt: 2 };
    let releaseOldWrite!: () => void;
    let stored: AssistantComposerDraft | null = null;
    const gate = new Promise<void>((resolve) => { releaseOldWrite = resolve; });
    const durable: AssistantDraftStorage = {
      get: vi.fn().mockImplementation(async () => stored), list: vi.fn().mockImplementation(async () => stored ? [stored] : []),
      put: vi.fn().mockImplementation(async (_scope, draft: AssistantComposerDraft) => {
        if (draft.content === old.content) await gate;
        stored = draft;
      }),
      remove: vi.fn().mockImplementation(async () => { stored = null; }),
    };
    const storage = new ResilientAssistantDraftStorage(durable);
    const oldWrite = storage.put('scope', old);
    const removal = storage.remove('scope', old.id);
    const newWrite = storage.put('scope', updated);
    releaseOldWrite();
    await Promise.all([oldWrite, removal, newWrite]);
    expect(stored).toEqual(updated);
    expect(await storage.get('scope', old.id)).toEqual(updated);
  });

  it('keeps the original draft readable when its durable removal fails', async () => {
    const draft: AssistantComposerDraft = { id: 'local:one', conversationID: null, content: '保留的草稿',
      modelID: '', hostID: '', mcpConnectorIDs: [], attachments: [], updatedAt: 1 };
    const durable: AssistantDraftStorage = {
      get: vi.fn().mockResolvedValue(draft), list: vi.fn().mockResolvedValue([draft]),
      put: vi.fn().mockResolvedValue(undefined), remove: vi.fn().mockRejectedValue(new Error('delete failed')),
    };
    const storage = new ResilientAssistantDraftStorage(durable);
    await storage.put('scope', draft);
    await expect(storage.remove('scope', draft.id)).rejects.toThrow('delete failed');
    expect(await storage.get('scope', draft.id)).toEqual(draft);
    expect(await storage.list('scope')).toEqual([draft]);
  });

  it('does not restore a failed removal over a newer draft', async () => {
    const old: AssistantComposerDraft = { id: 'local:one', conversationID: null, content: '旧草稿',
      modelID: '', hostID: '', mcpConnectorIDs: [], attachments: [], updatedAt: 1 };
    const updated = { ...old, content: '新草稿', updatedAt: 2 };
    let releaseRemoval!: () => void;
    let stored: AssistantComposerDraft | null = null;
    const gate = new Promise<void>((resolve) => { releaseRemoval = resolve; });
    const durable: AssistantDraftStorage = {
      get: vi.fn().mockImplementation(async () => stored), list: vi.fn().mockImplementation(async () => stored ? [stored] : []),
      put: vi.fn().mockImplementation(async (_scope, draft: AssistantComposerDraft) => { stored = draft; }),
      remove: vi.fn().mockImplementation(async () => { await gate; throw new Error('delete failed'); }),
    };
    const storage = new ResilientAssistantDraftStorage(durable);
    await storage.put('scope', old);
    const removal = storage.remove('scope', old.id).catch((error: Error) => error);
    const newWrite = storage.put('scope', updated);
    releaseRemoval();
    expect(await removal).toEqual(new Error('delete failed'));
    await newWrite;
    expect(stored).toEqual(updated);
    expect(await storage.get('scope', old.id)).toEqual(updated);
  });
});
