import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { Link } from 'react-router';
import { newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { catalogue } from '../../../../project/nanoMuse/web/src/ideas/index.js';
import {
  getPersonalDataSyncEnabled,
  PersonalDataStore,
  personalObjectKey,
  setPersonalDataSyncEnabled,
  sha256Hex,
  subscribePersonalDataSyncEnabled,
  synchronizePersonalData,
  type PersonalDataAPI,
  type PersonalDataState,
  type PersonalObject,
  type PersonalObjectBody,
  type PersonalObjectType,
} from './personal-data-store';
import { batchLegacyHistory, legacyHistorySource, readLegacyHistorySelection, type LegacyHistorySelection } from './personal-history-import';

type Section = 'profile' | 'memory' | 'feed' | 'ideas' | 'library';
type ProfileBody = { name: string; avatar: 'dragon' | 'emoji'; emoji?: string; color?: string; description?: string; style?: string };
type MemoryBody = { content: string; category?: string };
type FeedPostBody = { title: string; type: string; emoji?: string; created_at?: string; liked?: boolean; body: string };
type LibraryBody = { name: string; kind: 'artifact' | 'media'; mime?: string; size_bytes?: number; attachment_id?: string };

const sections: Array<{ id: Section; title: string }> = [
  { id: 'profile', title: '个人资料' }, { id: 'memory', title: '记忆' }, { id: 'feed', title: '动态' },
  { id: 'ideas', title: '灵感' }, { id: 'library', title: '资源库' },
];

const profileStyles = [
  ['', '默认'], ['muse', 'Muse'], ['flat', '扁平'], ['clay', '黏土'],
  ['watercolor', '水彩'], ['pixel', '像素'], ['line', '线稿'], ['sticker', '贴纸'],
] as const;

const personalAPI = portalApi as unknown as PersonalDataAPI;

function objectsWithDrafts(state: PersonalDataState): PersonalObject[] {
  const objects = new Map(Object.entries(state.objects));
  for (const mutation of [...state.outbox].sort((left, right) => left.createdAt - right.createdAt)) {
    const key = personalObjectKey(mutation.objectType, mutation.objectID);
    if (mutation.operation === 'delete') objects.delete(key);
    else if (mutation.body) objects.set(key, {
      object_type: mutation.objectType, object_id: mutation.objectID,
      revision: mutation.baseRevision, body: mutation.body,
    });
  }
  return [...objects.values()];
}

function objectOf<T extends PersonalObjectBody>(objects: PersonalObject[], type: PersonalObjectType, id: string): (PersonalObject & { body: T }) | undefined {
  return objects.find((item) => item.object_type === type && item.object_id === id) as (PersonalObject & { body: T }) | undefined;
}

function profileFrom(value: PersonalObject | undefined, fallbackName: string): ProfileBody {
  const body = value?.body;
  return {
    name: typeof body?.name === 'string' ? body.name : fallbackName,
    avatar: body?.avatar === 'emoji' ? 'emoji' : 'dragon',
    emoji: typeof body?.emoji === 'string' ? body.emoji : '',
    color: typeof body?.color === 'string' ? body.color : '#E8B85A',
    description: typeof body?.description === 'string' ? body.description : '',
    style: typeof body?.style === 'string' ? body.style : '',
  };
}

function randomMemoryID(existing: Set<string>): string {
  let candidate: string;
  do {
    const bytes = new Uint8Array(4);
    globalThis.crypto.getRandomValues(bytes);
    candidate = `m_${[...bytes].map((byte) => byte.toString(16).padStart(2, '0')).join('')}`;
  } while (existing.has(candidate));
  return candidate;
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function downloadBlob(blob: Blob, name: string): void {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = name;
  anchor.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

export function PersonalData() {
  const { user } = useAuth();
  const userID = user?.id;
  const store = useMemo(() => userID ? new PersonalDataStore(userID) : null, [userID]);
  const [state, setState] = useState<PersonalDataState | null>(null);
  const [activeSection, setActiveSection] = useState<Section>('profile');
  const [localError, setLocalError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [syncEnabled, setSyncEnabled] = useState(() => user ? getPersonalDataSyncEnabled(user.id) : true);
  const [syncing, setSyncing] = useState(false);
  const [quota, setQuota] = useState<{ used_bytes: number; remaining_bytes: number; quota_bytes: number; max_attachment_bytes: number } | null>(null);
  const [quotaError, setQuotaError] = useState<string | null>(null);
  const [memoryContent, setMemoryContent] = useState('');
  const [memoryCategory, setMemoryCategory] = useState('');
  const [feedTitle, setFeedTitle] = useState('');
  const [feedBody, setFeedBody] = useState('');
  const [libraryKind, setLibraryKind] = useState<'artifact' | 'media'>('artifact');
  const [historySelection, setHistorySelection] = useState<LegacyHistorySelection | null>(null);
  const [historyImporting, setHistoryImporting] = useState(false);
  const [historyImportError, setHistoryImportError] = useState<string | null>(null);
  const [historyImportStatus, setHistoryImportStatus] = useState<string | null>(null);
  const syncingRef = useRef(false);

  const refreshLocal = useCallback(async () => {
    if (!store) return;
    try {
      setState(await store.read());
      setLocalError(null);
    } catch (error) {
      setLocalError(error instanceof Error ? error.message : '无法读取本地个人数据。');
    }
  }, [store]);

  const sync = useCallback(async () => {
    if (!store || !getPersonalDataSyncEnabled(store.userID, store.serviceOrigin) || syncingRef.current) return;
    syncingRef.current = true;
    setSyncing(true);
    try {
      await synchronizePersonalData(store, personalAPI);
      if (getPersonalDataSyncEnabled(store.userID, store.serviceOrigin) && (typeof navigator === 'undefined' || navigator.onLine)) {
        try {
          const result = await personalAPI.personalDataQuota();
          setQuota({ used_bytes: result.used_bytes, remaining_bytes: result.remaining_bytes, quota_bytes: result.quota_bytes, max_attachment_bytes: result.max_attachment_bytes });
          setQuotaError(null);
        } catch (error) {
          setQuotaError(error instanceof Error ? error.message : '无法读取附件空间。');
        }
      }
    } finally {
      syncingRef.current = false;
      setSyncing(false);
      await refreshLocal();
    }
  }, [store, refreshLocal]);

  useEffect(() => {
    if (!store) return undefined;
    let alive = true;
    const unsubscribeStore = store.subscribe((next) => { if (alive) setState(next); });
    const unsubscribeSetting = subscribePersonalDataSyncEnabled(store.userID, (enabled) => {
      if (!alive) return;
      setSyncEnabled(enabled);
      if (enabled) void sync();
    }, store.serviceOrigin);
    const onOnline = () => { if (getPersonalDataSyncEnabled(store.userID, store.serviceOrigin)) void sync(); };
    window.addEventListener('online', onOnline);
    void store.read().then((next) => {
      if (!alive) return;
      setState(next);
      setLocalError(null);
      if (getPersonalDataSyncEnabled(store.userID, store.serviceOrigin)) void sync();
    }).catch((error: unknown) => {
      if (alive) setLocalError(error instanceof Error ? error.message : '无法读取本地个人数据。');
    });
    return () => {
      alive = false;
      unsubscribeStore();
      unsubscribeSetting();
      window.removeEventListener('online', onOnline);
    };
  }, [store, sync]);

  const allObjects = useMemo(() => state ? objectsWithDrafts(state) : [], [state]);
  const profileObject = objectOf<ProfileBody>(allObjects, 'profile', 'default');
  const memoryObjects = allObjects.filter((item) => item.object_type === 'memory');
  const feedObjects = allObjects.filter((item) => item.object_type === 'feed_post').sort((a, b) => asString(b.body.created_at).localeCompare(asString(a.body.created_at)));
  const ideaMarks = new Map(allObjects.filter((item) => item.object_type === 'idea_mark').map((item) => [item.object_id, asString(item.body.state)]));
  const libraryObjects = allObjects.filter((item) => item.object_type === 'library_item');
  const feedPreference = objectOf<{ content: string }>(allObjects, 'feed_preferences', 'default');
  const conflicts = useMemo(() => state?.outbox.filter((item) => item.status === 'conflict' && !item.resolutionQueued) ?? [], [state?.outbox]);
  const archivedConflicts = useMemo(() => state?.outbox.filter((item) => item.status === 'conflict' && item.resolutionQueued) ?? [], [state?.outbox]);
  const pendingCount = (state?.outbox.filter((item) => item.status === 'pending').length ?? 0) + (state?.attachments.filter((item) => item.status === 'pending').length ?? 0);
  const remoteLoading = Boolean(state && syncEnabled && !state.snapshotComplete && !state.syncError && (typeof navigator === 'undefined' || navigator.onLine));

  const saveObject = useCallback(async (type: PersonalObjectType, id: string, body: PersonalObjectBody) => {
    if (!store) return;
    setActionError(null);
    try {
      const conflict = conflicts.find((entry) => entry.objectType === type && entry.objectID === id);
      if (conflict) await store.rebaseDraft(conflict.id, body);
      else await store.queueReplace(type, id, body);
      if (syncEnabled) void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法保存个人数据。');
    }
  }, [store, conflicts, syncEnabled, sync]);

  const deleteObject = useCallback(async (type: PersonalObjectType, id: string) => {
    if (!store) return;
    setActionError(null);
    try {
      await store.queueDelete(type, id);
      if (syncEnabled) void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法删除个人数据。');
    }
  }, [store, syncEnabled, sync]);

  const selectLegacyHistory = async (files: File[]) => {
    setHistoryImportError(null);
    setHistoryImportStatus(null);
    if (!files.length) {
      setHistorySelection(null);
      return;
    }
    try {
      setHistorySelection(await readLegacyHistorySelection(files));
    } catch (error) {
      setHistorySelection(null);
      setHistoryImportError(error instanceof Error ? error.message : '无法读取所选会话文件。');
    }
  };

  const importLegacyHistory = async () => {
    if (!historySelection || historyImporting) return;
    setHistoryImportError(null);
    setHistoryImporting(true);
    let completedMessages = 0;
    let completedConversations = 0;
    const totalConversations = historySelection.conversations.length;
    try {
      for (const conversation of historySelection.conversations) {
        let targetConversationID: string | undefined;
        for (const messages of batchLegacyHistory(conversation.messages)) {
          const receipt = await portalApi.importPersonalHistory({
            ...legacyHistorySource,
            source_conversation_id: conversation.source_conversation_id,
            title: conversation.title,
            ...(targetConversationID ? { target_conversation_id: targetConversationID } : {}),
            messages,
          });
          targetConversationID = receipt.conversation_id;
          completedMessages += messages.length;
          setHistoryImportStatus(`正在导入会话 ${completedConversations + 1}/${totalConversations}，已提交 ${completedMessages} 条文本消息…`);
        }
        completedConversations++;
      }
      setHistoryImportStatus(`已导入 ${completedConversations} 个会话、${completedMessages} 条文本消息。重复导入会按来源消息去重。`);
    } catch (error) {
      setHistoryImportError(`导入已中断；已提交 ${completedMessages} 条文本消息。${error instanceof Error ? error.message : '请稍后重试。'} 同一文件可安全重试。`);
    } finally {
      setHistoryImporting(false);
    }
  };

  const addMemory = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const content = memoryContent.trim();
    if (!content) return;
    const ids = new Set(memoryObjects.map((item) => item.object_id));
    const id = randomMemoryID(ids);
    void saveObject('memory', id, { content, ...(memoryCategory.trim() ? { category: memoryCategory.trim() } : {}) });
    setMemoryContent('');
    setMemoryCategory('');
  };

  const addFeedPost = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const title = feedTitle.trim();
    const body = feedBody.trim();
    if (!title || !body) return;
    void saveObject('feed_post', newIdempotencyKey(), {
      title, type: 'note', created_at: new Date().toISOString(), liked: false, body,
    });
    setFeedTitle('');
    setFeedBody('');
  };

  const saveFeedPreferences = (content: string) => {
    if (!content.trim()) {
      if (feedPreference) void deleteObject('feed_preferences', 'default');
      return;
    }
    void saveObject('feed_preferences', 'default', { content: content.trim() });
  };

  const toggleIdea = (ideaID: string) => {
    const next = ideaMarks.get(ideaID) === 'saved' ? 'dismissed' : 'saved';
    void saveObject('idea_mark', ideaID, { state: next });
  };

  const chooseFile = async (file: File, target?: PersonalObject) => {
    if (!store) return;
    setActionError(null);
    try {
      const hasControlCharacter = [...file.name].some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127);
      if (!file.name || file.name === '.' || file.name === '..' || [...file.name].length > 255 || file.name.includes('/') || file.name.includes('\\') || hasControlCharacter) {
        throw new Error('文件名需为 1 到 255 个字符，且不能包含斜杠或控制字符。');
      }
      if (file.type.length > 128) throw new Error('文件媒体类型过长，无法保存。');
      if (quota && file.size > quota.max_attachment_bytes) throw new Error(`单个附件不能超过 ${formatBytes(quota.max_attachment_bytes)}。`);
      if (syncEnabled && quota && file.size > quota.remaining_bytes) throw new Error('附件空间不足，请先清理资源库内容。');
      const kind: LibraryBody['kind'] = target ? (target.body.kind === 'media' ? 'media' : 'artifact') : libraryKind;
      const objectID = target?.object_id ?? newIdempotencyKey();
      const mediaType = file.type || 'application/octet-stream';
      const digest = await sha256Hex(await file.arrayBuffer());
      const baseRevision = target?.revision ?? 0;
      if (!target) {
        await store.queueLibraryAttachment({
          objectID, name: file.name, kind, mediaType, sizeBytes: file.size, sha256: digest, blob: file,
        }, {
          name: file.name, kind, mime: mediaType, size_bytes: file.size,
        });
      } else {
        await store.queueAttachment({
          objectID, name: file.name, kind, mediaType, sizeBytes: file.size, sha256: digest, blob: file, baseRevision,
        });
      }
      if (syncEnabled) void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法保存附件草稿。');
    }
  };

  const downloadAttachment = async (item: PersonalObject & { body: LibraryBody }) => {
    const attachmentID = item.body.attachment_id;
    if (!attachmentID || !store) return;
    setActionError(null);
    try {
      const cached = await store.cachedAttachment(attachmentID);
      if (cached) { downloadBlob(cached.blob, cached.name || item.body.name); return; }
      if (typeof navigator !== 'undefined' && navigator.onLine === false) throw new Error('当前离线，且此附件尚未缓存。联网后重试。');
      const blob = await personalAPI.personalAttachment(attachmentID);
      await store.cacheAttachment({ attachmentID, name: item.body.name, mediaType: item.body.mime ?? blob.type, blob });
      downloadBlob(blob, item.body.name);
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法读取附件。');
    }
  };

  const rebaseConflict = async (mutationID: string) => {
    if (!store) return;
    try {
      await store.rebaseDraft(mutationID);
      if (syncEnabled) void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法基于当前版本保存草稿。');
    }
  };

  const rebaseAttachment = async (id: string, objectID: string) => {
    if (!store) return;
    const remote = state?.objects[personalObjectKey('library_item', objectID)];
    if (!remote || remote.body.attachment_id) {
      setActionError('远端资源不存在或已有附件，不能重试上传。');
      return;
    }
    try {
      await store.rebaseAttachment(id, remote.revision);
      if (syncEnabled) void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法重新排队附件。');
    }
  };

  if (!user) return <section><div className="loading-card" role="status">正在读取账号…</div></section>;

  return <section className="personal-data-page">
    <div className="page-heading">
      <div><p className="eyebrow">我的数据</p><h1>个人资料</h1><p className="muted">管理个人资料、记忆、动态、灵感标记和资源库附件。</p></div>
      <label className="checkbox-row personal-sync-toggle"><input type="checkbox" checked={syncEnabled} onChange={(event) => {
        const enabled = event.target.checked;
        setPersonalDataSyncEnabled(user.id, enabled, store?.serviceOrigin);
        setSyncEnabled(enabled);
        if (enabled) void sync();
      }} />同步到 Morde</label>
    </div>
    <p className="notice notice-info" role="note">其他旧版本机资料不会自动导入或删除；只有你明确选择并提交的会话文本才会导入当前账号。未选中的资料、工具输出和附件继续留在原导出文件中。</p>

    <section className="connection-section" aria-labelledby="legacy-history-import-title">
      <div className="connection-section-heading"><div><h2 id="legacy-history-import-title">导入 nanoMuse 会话历史</h2><p className="muted">从旧版下载的数据 ZIP 解压后，可在 threads 文件夹选择一个或多个会话 JSON；index.json 可一并选择以使用原会话标题。</p></div></div>
      <label htmlFor="legacy-history-files">选择要导入的会话 JSON 文件</label>
      <input id="legacy-history-files" type="file" accept=".json,application/json" multiple disabled={historyImporting}
        onChange={(event) => void selectLegacyHistory(Array.from(event.currentTarget.files ?? []))} />
      <p className="muted">导入前会显示会话与消息数量。只发送可见的用户和助手文本；推理、工具事件、后台消息和旧附件不会导入，也不会创建任务。</p>
      {historySelection && <div className="empty-card" role="status">
        <p>可导入 {historySelection.conversations.length} 个会话、{historySelection.conversations.reduce((total, item) => total + item.messages.length, 0)} 条文本消息；跳过 {historySelection.skipped_files} 个文件、{historySelection.ignored_events} 个不支持事件、{historySelection.skipped_attachments} 个附件引用。</p>
        {!!historySelection.conversations.length && <ul>{historySelection.conversations.map((item) => <li key={item.source_conversation_id}>{item.title} · {item.messages.length} 条消息</li>)}</ul>}
      </div>}
      {historyImportError && <p className="error-card" role="alert">{historyImportError}</p>}
      {historyImportStatus && <p className="notice notice-info" role="status">{historyImportStatus}</p>}
      <button type="button" className="button button-secondary" disabled={!historySelection?.conversations.length || historyImporting}
        onClick={() => void importLegacyHistory()}>{historyImporting ? '正在导入…' : `明确导入所选历史（${historySelection?.conversations.reduce((total, item) => total + item.messages.length, 0) ?? 0} 条）`}</button>
    </section>

    <div className="connection-section-heading personal-sync-status" aria-live="polite">
      <p className="muted">{syncing ? '正在同步个人数据…' : syncEnabled ? '同步已开启' : '同步已关闭；本地数据会继续保留'} · {pendingCount} 项待同步 · {conflicts.length} 条冲突草稿</p>
      <button type="button" className="button button-secondary" disabled={!syncEnabled || syncing} onClick={() => void sync()}>{syncing ? '同步中…' : '立即同步'}</button>
    </div>

    {localError && <div className="error-card" role="alert">{localError}<button type="button" className="button button-secondary" onClick={() => void refreshLocal()}>重试</button></div>}
    {state?.syncError && <div className="error-card" role="alert">同步未完成：{state.syncError} 本地草稿和附件仍保留。<button type="button" className="button button-secondary" disabled={!syncEnabled || syncing} onClick={() => void sync()}>重试同步</button></div>}
    {actionError && <div className="error-inline" role="alert">{actionError}<button type="button" className="icon-button" aria-label="关闭提示" onClick={() => setActionError(null)}>×</button></div>}
    {!state && !localError && <div className="loading-card" role="status">正在读取本地个人数据…</div>}

    {!!conflicts.length && <section className="notice notice-warning" aria-label="保留的冲突草稿">
      <h2>有 {conflicts.length} 条草稿需要处理</h2>
      <p>服务端版本已单独保留。你可以查看远端版本，再明确选择基于当前版本另存草稿；原冲突草稿会继续保留。</p>
      <ul>{conflicts.map((item) => {
        const remote = state?.objects[personalObjectKey(item.objectType, item.objectID)];
        return <li key={item.id}>
          <strong>{item.objectType} / {item.objectID}</strong>：{item.message ?? '版本冲突'}
          {remote && <span> 远端版本 {remote.revision}：{JSON.stringify(remote.body).slice(0, 180)}</span>}
          {!item.resolutionQueued && <button type="button" className="button button-small button-secondary" onClick={() => void rebaseConflict(item.id)}>{item.operation === 'delete' ? '基于当前远端版本重试删除' : '基于当前远端版本另存此草稿'}</button>}
        </li>;
      })}</ul>
    </section>}
    {!!archivedConflicts.length && <details className="notice notice-info">
      <summary>已另存处理、仍保留原文的冲突草稿（{archivedConflicts.length}）</summary>
      <ul>{archivedConflicts.map((item) => <li key={item.id}>
        <strong>{item.objectType} / {item.objectID}</strong>：{JSON.stringify(item.body).slice(0, 240)}
      </li>)}</ul>
    </details>}

    <div className="connection-section-heading personal-data-tabs" role="tablist" aria-label="个人数据分类">
      {sections.map((section) => <button key={section.id} type="button" role="tab" aria-selected={activeSection === section.id}
        className={`button ${activeSection === section.id ? 'button-primary' : 'button-secondary'}`} onClick={() => setActiveSection(section.id)}>{section.title}</button>)}
    </div>

    {remoteLoading && <div className="loading-card" role="status">正在读取服务端个人数据；本地草稿仍可离线访问。</div>}

    {state && !remoteLoading && activeSection === 'profile' && <section className="connection-section" aria-labelledby="personal-profile-title">
      <div className="connection-section-heading"><div><h2 id="personal-profile-title">个人资料</h2><p className="muted">这里只保存服务端允许的资料字段。</p></div></div>
      {!profileObject && <p className="empty-card">还没有个人资料。保存后会创建默认资料。</p>}
      <ProfileEditor key={`${profileObject?.revision ?? 0}:${JSON.stringify(profileObject?.body ?? null)}`} value={profileObject} fallbackName={user?.name ?? ''}
        hasConflict={conflicts.some((item) => item.objectType === 'profile' && item.objectID === 'default')}
        onSave={(body) => void saveObject('profile', 'default', body as unknown as PersonalObjectBody)} onInvalid={setActionError} />
    </section>}

    {state && !remoteLoading && activeSection === 'memory' && <section className="connection-section" aria-labelledby="personal-memory-title">
      <div className="connection-section-heading"><div><h2 id="personal-memory-title">记忆</h2><p className="muted">记忆以独立版本保存，可离线新增、修改和删除。</p></div></div>
      <form className="form-card form-stack" onSubmit={addMemory}>
        <label>新增记忆<textarea value={memoryContent} maxLength={65536} required onChange={(event) => setMemoryContent(event.target.value)} /></label>
        <label>分类（可选）<input value={memoryCategory} maxLength={64} onChange={(event) => setMemoryCategory(event.target.value)} /></label>
        <button className="button button-primary" type="submit">保存记忆</button>
      </form>
      {memoryObjects.length === 0 ? <p className="empty-card">还没有记忆。</p> : <ul className="personal-object-list">{memoryObjects.map((item) => <MemoryRow key={`${item.object_id}:${item.revision}:${JSON.stringify(item.body)}`} item={item} onSave={(body) => void saveObject('memory', item.object_id, body)} onDelete={() => void deleteObject('memory', item.object_id)} />)}</ul>}
    </section>}

    {state && !remoteLoading && activeSection === 'feed' && <section className="connection-section" aria-labelledby="personal-feed-title">
      <div className="connection-section-heading"><div><h2 id="personal-feed-title">动态</h2><p className="muted">个人动态保存在 feed_post 对象中；偏好设置与动态分开保存。</p></div></div>
      <FeedPreferencesEditor key={feedPreference?.body.content ?? 'empty'} initialContent={feedPreference?.body.content ?? ''} onSave={saveFeedPreferences} />
      <form className="form-card form-stack" onSubmit={addFeedPost}>
        <label>新增动态标题<input value={feedTitle} maxLength={200} required onChange={(event) => setFeedTitle(event.target.value)} /></label>
        <label>内容<textarea value={feedBody} maxLength={65536} required onChange={(event) => setFeedBody(event.target.value)} /></label>
        <button className="button button-primary" type="submit">发布到个人动态</button>
      </form>
      {feedObjects.length === 0 ? <p className="empty-card">还没有动态。</p> : <ul className="personal-object-list">{feedObjects.map((item) => {
        const body = item.body as FeedPostBody;
        return <li className="model-connection-card" key={item.object_id}>
          <div className="model-connection-main"><h3>{body.title}</h3><p className="muted">{body.created_at ? new Date(body.created_at).toLocaleString() : '时间未知'} · {body.type}</p><p>{body.body}</p></div>
          <div className="inline-actions"><button type="button" className="button button-secondary" onClick={() => void saveObject('feed_post', item.object_id, { ...body, liked: !body.liked })}>{body.liked ? '取消喜欢' : '喜欢'}</button><button type="button" className="button button-secondary" onClick={() => void deleteObject('feed_post', item.object_id)}>删除</button></div>
        </li>;
      })}</ul>}
    </section>}

    {state && !remoteLoading && activeSection === 'ideas' && <section className="connection-section" aria-labelledby="personal-ideas-title">
      <div className="connection-section-heading"><div><h2 id="personal-ideas-title">灵感</h2><p className="muted">灵感来自本地 nanoMuse 静态目录；服务端只保存你的收藏或忽略标记。</p></div></div>
      {catalogue('zh-CN').sections.map((group) => <section key={group.id} aria-label={group.title}>
        <h3>{group.title}</h3><ul className="personal-object-list">{group.ideas.map((idea) => {
          const mark = ideaMarks.get(idea.id);
          return <li className="model-connection-card" key={idea.id}>
            <div className="model-connection-main"><h3>{idea.emoji} {idea.title}</h3><p>{idea.body}</p><small className="muted">{mark === 'saved' ? '已收藏' : mark === 'dismissed' ? '已忽略' : '尚未标记'}</small></div>
            <div className="inline-actions"><button type="button" className="button button-secondary" onClick={() => toggleIdea(idea.id)}>{mark === 'saved' ? '取消收藏' : '收藏'}</button>
              {mark !== 'dismissed' && <button type="button" className="button button-secondary" onClick={() => void saveObject('idea_mark', idea.id, { state: 'dismissed' })}>忽略</button>}
              <Link className="button button-primary" to="/assistant" state={{ ideaID: idea.id, ideaPrompt: idea.prompt }}>带入助手</Link></div>
          </li>;
        })}</ul>
      </section>)}
    </section>}

    {state && !remoteLoading && activeSection === 'library' && <section className="connection-section" aria-labelledby="personal-library-title">
      <div className="connection-section-heading"><div><h2 id="personal-library-title">资源库</h2><p className="muted">附件字节保存在本机离线队列；联网后按 4 MiB 分块上传。</p></div></div>
      {quota && <p className="notice notice-info" role="status">附件空间已用 {formatBytes(quota.used_bytes)} / {formatBytes(quota.quota_bytes)}，剩余 {formatBytes(quota.remaining_bytes)}；单个附件上限 {formatBytes(quota.max_attachment_bytes)}。</p>}
      {quotaError && <div className="error-inline" role="alert">无法读取附件空间：{quotaError}<button type="button" className="button button-small button-secondary" onClick={() => void sync()}>重试</button></div>}
      <form className="form-card form-stack" onSubmit={(event) => event.preventDefault()}>
        <label>资源类型<select value={libraryKind} onChange={(event) => setLibraryKind(event.target.value as LibraryBody['kind'])}><option value="artifact">文档/产物</option><option value="media">媒体</option></select></label>
        <label>上传新资源<input type="file" onChange={(event) => {
          const file = event.currentTarget.files?.[0];
          if (file) void chooseFile(file);
          event.currentTarget.value = '';
        }} /></label>
      </form>
      {libraryObjects.length === 0 ? <p className="empty-card">资源库还是空的。可以先添加本机文件。</p> : <ul className="personal-object-list">{libraryObjects.map((item) => {
        const body = item.body as LibraryBody;
        const attachment = state.attachments.find((entry) => entry.objectID === item.object_id);
        return <li className="model-connection-card" key={item.object_id}>
          <div className="model-connection-main"><h3>{body.name}</h3><p className="muted">{body.kind === 'media' ? '媒体' : '文档/产物'} · {formatBytes(body.size_bytes ?? 0)} · {body.mime ?? '未知格式'}</p>
            {attachment && <p role="status">{attachment.status === 'conflict' ? `附件冲突：${attachment.message ?? '草稿已保留'}` : `附件已保存在本机，${syncEnabled ? '等待上传或正在上传' : '同步关闭时不会上传'}`}</p>}</div>
          <div className="inline-actions">{body.attachment_id && <button type="button" className="button button-secondary" onClick={() => void downloadAttachment(item as PersonalObject & { body: LibraryBody })}>下载附件</button>}
            {!body.attachment_id && !attachment && <label className="button button-secondary">添加附件<input className="sr-only" type="file" onChange={(event) => {
              const file = event.currentTarget.files?.[0];
              if (file) void chooseFile(file, item);
              event.currentTarget.value = '';
            }} /></label>}
            <button type="button" className="button button-secondary" onClick={() => void deleteObject('library_item', item.object_id)}>删除</button></div>
        </li>;
      })}</ul>}
      {!!state.attachments.filter((item) => item.status === 'conflict').length && <section className="notice notice-warning"><h3>附件草稿冲突</h3>{state.attachments.filter((item) => item.status === 'conflict').map((item) => <p key={item.id}>{item.name}：{item.message} <button type="button" className="button button-small button-secondary" onClick={() => void rebaseAttachment(item.id, item.objectID)}>保留草稿并基于远端重试</button></p>)}</section>}
    </section>}
  </section>;
}

function ProfileEditor({ value, fallbackName, hasConflict, onSave, onInvalid }: {
  value: PersonalObject | undefined;
  fallbackName: string;
  hasConflict: boolean;
  onSave: (body: ProfileBody) => void;
  onInvalid: (message: string) => void;
}) {
  const [profile, setProfile] = useState(() => profileFrom(value, fallbackName));
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const name = profile.name.trim();
    if (!name) { onInvalid('名称不能为空。'); return; }
    onSave({
      name, avatar: profile.avatar,
      ...(profile.avatar === 'emoji' ? { emoji: profile.emoji?.trim(), color: profile.color } : { emoji: '', color: '' }),
      description: profile.description?.trim() ?? '', style: profile.style ?? '',
    });
  };
  return <form className="form-card form-stack" onSubmit={save}>
    <label>名称<input value={profile.name} maxLength={60} required onChange={(event) => setProfile({ ...profile, name: event.target.value })} /></label>
    <label>头像<select value={profile.avatar} onChange={(event) => setProfile({ ...profile, avatar: event.target.value as ProfileBody['avatar'], emoji: '', color: '#E8B85A' })}><option value="dragon">龙</option><option value="emoji">表情</option></select></label>
    {profile.avatar === 'emoji' && <>
      <label>表情<input value={profile.emoji ?? ''} maxLength={16} required onChange={(event) => setProfile({ ...profile, emoji: event.target.value })} /></label>
      <label>颜色<input type="color" value={profile.color ?? '#E8B85A'} onChange={(event) => setProfile({ ...profile, color: event.target.value })} /></label>
    </>}
    <label>简介<textarea value={profile.description ?? ''} maxLength={200} onChange={(event) => setProfile({ ...profile, description: event.target.value })} /></label>
    <label>风格<select value={profile.style ?? ''} onChange={(event) => setProfile({ ...profile, style: event.target.value })}>{profileStyles.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
    <button type="submit" className="button button-primary">{hasConflict ? '基于远端版本保存草稿' : '保存个人资料'}</button>
  </form>;
}

function FeedPreferencesEditor({ initialContent, onSave }: { initialContent: string; onSave: (content: string) => void }) {
  const [content, setContent] = useState(initialContent);
  return <form className="form-card form-stack" onSubmit={(event) => { event.preventDefault(); onSave(content); }}>
    <label>动态偏好<textarea value={content} maxLength={8192} onChange={(event) => setContent(event.target.value)} placeholder="写下你想看到的动态内容" /></label>
    <button type="submit" className="button button-secondary">{content.trim() ? '保存动态偏好' : '清除动态偏好'}</button>
  </form>;
}

function MemoryRow({ item, onSave, onDelete }: { item: PersonalObject; onSave: (body: MemoryBody) => void; onDelete: () => void }) {
  const [content, setContent] = useState(asString(item.body.content));
  const [category, setCategory] = useState(asString(item.body.category));
  return <li className="model-connection-card">
    <div className="model-connection-main"><label>记忆内容<textarea value={content} maxLength={65536} onChange={(event) => setContent(event.target.value)} /></label><label>分类<input value={category} maxLength={64} onChange={(event) => setCategory(event.target.value)} /></label></div>
    <div className="inline-actions"><button type="button" className="button button-secondary" onClick={() => onSave({ content: content.trim(), ...(category.trim() ? { category: category.trim() } : {}) })}>保存修改</button><button type="button" className="button button-secondary" onClick={onDelete}>删除</button></div>
  </li>;
}

export default PersonalData;
