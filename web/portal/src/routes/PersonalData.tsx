import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { FormEvent } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useLocation } from 'react-router';
import { newIdempotencyKey, portalApi } from '../app/api';
import { useAuth } from '../app/auth';
import { ScreenPagination } from '../components/ScreenPagination';
import './PersonalData.css';
import { ideasFromFeedPreferences, personalIdeas } from '../data/ideas';
import {
  PersonalDataStore,
  personalObjectKey,
  sha256Hex,
  synchronizePersonalData,
  type PersonalDataAPI,
  type PersonalDataState,
  type PersonalObject,
  type PersonalObjectBody,
  type PersonalObjectType,
} from './personal-data-store';
import { FloatingError } from '../components/FloatingError';

type Section = 'profile' | 'soul' | 'memory' | 'feed' | 'ideas' | 'library' | 'preferences';
type ProfileBody = { name: string; avatar: 'dragon' | 'emoji'; emoji?: string; color?: string; description?: string; style?: string };
type SoulBody = { name: string; emoji?: string; style?: string; lang?: string; content: string };
type MemoryBody = { content: string; category?: string };
type FeedPostBody = { title: string; type: string; emoji?: string; created_at?: string; liked?: boolean; body: string };
type LibraryBody = { name: string; kind: 'artifact' | 'media'; mime?: string; size_bytes?: number; attachment_id?: string };
type GoalStepBody = { id?: unknown; title?: unknown; status?: unknown };
type GoalSuggestionBody = { title?: unknown; status?: unknown; steps?: unknown };
type GoalSuggestion = { id: string; title: string; stepLabel: string; stepTitle: string; prompt: string };

const sections: Array<{ id: Section; title: string }> = [
  { id: 'profile', title: '个人资料' }, { id: 'soul', title: 'AI 个性' }, { id: 'memory', title: '记忆' }, { id: 'feed', title: '动态' },
  { id: 'ideas', title: '灵感' }, { id: 'library', title: '资源库' }, { id: 'preferences', title: '偏好' },
];

const profileStyles = [
  ['', '默认'], ['muse', 'Muse'], ['flat', '扁平'], ['clay', '黏土'],
  ['watercolor', '水彩'], ['pixel', '像素'], ['line', '线稿'], ['sticker', '贴纸'],
] as const;

function personalAPIFor(serviceInstanceID: string | undefined, actorID: string | undefined): PersonalDataAPI {
  const base = portalApi as unknown as PersonalDataAPI;
  if (!serviceInstanceID || !actorID) return base;
  return {
    ...base,
    replacePersonalObject: (type, id, baseRevision, body, key) => portalApi.replacePersonalObject(
      type as Parameters<typeof portalApi.replacePersonalObject>[0], id, baseRevision,
      body as Parameters<typeof portalApi.replacePersonalObject>[3], key, actorID, serviceInstanceID,
    ),
    deletePersonalObject: (type, id, baseRevision, key) => portalApi.deletePersonalObject(
      type as Parameters<typeof portalApi.deletePersonalObject>[0], id, baseRevision, key, serviceInstanceID, actorID,
    ),
    createPersonalAttachmentUpload: (body, key) => portalApi.createPersonalAttachmentUpload(
      body as Parameters<typeof portalApi.createPersonalAttachmentUpload>[0], key, serviceInstanceID, actorID,
    ),
    putPersonalAttachmentChunk: (uploadID, sequence, body, sha256) => portalApi.putPersonalAttachmentChunk(
      uploadID, sequence, body, sha256, serviceInstanceID, actorID,
    ),
    commitPersonalAttachment: (uploadID, key) => portalApi.commitPersonalAttachment(uploadID, key, serviceInstanceID, actorID),
    cancelPersonalAttachmentUpload: (uploadID) => portalApi.cancelPersonalAttachmentUpload(uploadID, serviceInstanceID, actorID),
  };
}

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

function goalSuggestionsFrom(objects: PersonalObject[]): GoalSuggestion[] {
  return objects.flatMap((item) => {
    if (item.object_type !== 'goal') return [];
    const body = item.body as GoalSuggestionBody;
    if (body.status !== 'active' || typeof body.title !== 'string' || !Array.isArray(body.steps)) return [];
    const steps = body.steps as GoalStepBody[];
    const step = steps.find((candidate) => candidate.status === 'blocked' &&
      typeof candidate.title === 'string' && candidate.title.trim()) ??
      steps.find((candidate) => candidate.status === 'in_progress' && typeof candidate.title === 'string' && candidate.title.trim()) ??
      steps.find((candidate) => candidate.status === 'pending' &&
      typeof candidate.title === 'string' && candidate.title.trim());
    if (!step || typeof step.title !== 'string') return [];
    const title = body.title.trim();
    const stepTitle = step.title.trim();
    if (!title || !stepTitle) return [];
    const stepLabel = step.status === 'blocked' ? '受阻步骤' : '当前步骤';
    return [{
      id: item.object_id,
      title,
      stepLabel,
      stepTitle,
      prompt: step.status === 'blocked'
        ? `请根据我的目标“${title}”帮我处理受阻步骤“${stepTitle}”。先检查已有记录并提出可行的解决办法；涉及修改、发送或其他需要确认的操作时，先向我说明并等待确认。`
        : `请根据我的目标“${title}”继续推进当前步骤“${stepTitle}”。先结合已有记录和可用工具给出下一步；涉及修改、发送或其他需要确认的操作时，先向我说明并等待确认。`,
    }];
  }).slice(0, 5);
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
  const location = useLocation();
  const userID = user?.id;
  const capabilities = useQuery({ queryKey: ['service-capabilities'], queryFn: portalApi.capabilities });
  const serviceInstanceID = capabilities.data?.service_instance_id;
  const personalAPI = useMemo(() => personalAPIFor(serviceInstanceID, userID), [serviceInstanceID, userID]);
  const store = useMemo(() => userID ? new PersonalDataStore(userID, window.location.origin, undefined, serviceInstanceID) : null, [userID, serviceInstanceID]);
  const [state, setState] = useState<PersonalDataState | null>(null);
  const [otherInstanceDrafts, setOtherInstanceDrafts] = useState<Array<{ scope: string; serviceInstanceID: string | null; state: PersonalDataState }>>([]);
  const requestedSection = new URLSearchParams(location.search).get('section');
  const activeSection: Section = sections.some((section) => section.id === requestedSection) ? requestedSection as Section : 'profile';
  const [localError, setLocalError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [confirmOldDelete, setConfirmOldDelete] = useState<string | null>(null);
  const [syncing, setSyncing] = useState(false);
  const [quota, setQuota] = useState<{ used_bytes: number; remaining_bytes: number; quota_bytes: number; max_attachment_bytes: number } | null>(null);
  const [quotaError, setQuotaError] = useState<string | null>(null);
  const [memoryContent, setMemoryContent] = useState('');
  const [memoryCategory, setMemoryCategory] = useState('');
  const [feedTitle, setFeedTitle] = useState('');
  const [feedBody, setFeedBody] = useState('');
  const [libraryKind, setLibraryKind] = useState<'artifact' | 'media'>('artifact');
  const syncingRef = useRef(false);
  const syncRequestedRef = useRef(false);

  const refreshOtherInstanceDrafts = useCallback(async () => {
    if (!store || !serviceInstanceID) { setOtherInstanceDrafts([]); return; }
    try {
      await store.quarantineOtherInstanceDrafts();
      setOtherInstanceDrafts(await store.otherInstanceDraftScopes());
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法读取其他服务实例保留的草稿。');
    }
  }, [store, serviceInstanceID]);

  const refreshLocal = useCallback(async () => {
    if (!store) return;
    try {
      setState(await store.read());
      setLocalError(null);
      await refreshOtherInstanceDrafts();
    } catch (error) {
      setLocalError(error instanceof Error ? error.message : '无法读取本地个人数据。');
    }
  }, [store, refreshOtherInstanceDrafts]);

  const sync = useCallback(async () => {
    if (!store || !serviceInstanceID) return;
    if (syncingRef.current) {
      syncRequestedRef.current = true;
      return;
    }
    syncingRef.current = true;
    setSyncing(true);
    try {
      await synchronizePersonalData(store, personalAPI);
      if (serviceInstanceID && (typeof navigator === 'undefined' || navigator.onLine)) {
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
      if (syncRequestedRef.current) {
        syncRequestedRef.current = false;
        void sync();
      }
    }
  }, [store, refreshLocal, personalAPI, serviceInstanceID]);

  useEffect(() => {
    if (!store) return undefined;
    let alive = true;
    const unsubscribeStore = store.subscribe((next) => { if (alive) setState(next); });
    const onOnline = () => { void sync(); };
    window.addEventListener('online', onOnline);
    void store.read().then((next) => {
      if (!alive) return;
      setState(next);
      setLocalError(null);
      void refreshOtherInstanceDrafts().finally(() => { if (alive) void sync(); });
    }).catch((error: unknown) => {
      if (alive) setLocalError(error instanceof Error ? error.message : '无法读取本地个人数据。');
    });
    return () => {
      alive = false;
      unsubscribeStore();
      window.removeEventListener('online', onOnline);
    };
  }, [store, sync, refreshOtherInstanceDrafts]);

  const allObjects = useMemo(() => state ? objectsWithDrafts(state) : [], [state]);
  const goalSuggestions = useMemo(() => goalSuggestionsFrom(allObjects), [allObjects]);
  const profileObject = objectOf<ProfileBody>(allObjects, 'profile', 'default');
  const soulObject = objectOf<SoulBody>(allObjects, 'soul', 'default');
  const memoryObjects = allObjects.filter((item) => item.object_type === 'memory');
  // Show the confirmed server value, not an offline draft, for this consent.
  const memorySetting = state?.objects[personalObjectKey('skill_setting', 'builtin_memory')];
  const memoryEnabled = memorySetting ? memorySetting.body.enabled === true : state?.snapshotComplete === true;
  const memorySettingPending = state?.outbox.some((item) => item.objectType === 'skill_setting' && item.objectID === 'builtin_memory' && !item.resolutionQueued) ?? false;
  const ideasSetting = state?.objects[personalObjectKey('skill_setting', 'builtin_ideas')];
  const ideasEnabled = ideasSetting ? ideasSetting.body.enabled === true : state?.snapshotComplete === true;
  const ideasSettingPending = state?.outbox.some((item) => item.objectType === 'skill_setting' && item.objectID === 'builtin_ideas' && !item.resolutionQueued) ?? false;
  const goalFollowupsSetting = state?.objects[personalObjectKey('skill_setting', 'builtin_goal_followups')];
  const goalFollowupsEnabled = goalFollowupsSetting?.body.enabled === true;
  const goalFollowupsSettingPending = state?.outbox.some((item) => item.objectType === 'skill_setting' && item.objectID === 'builtin_goal_followups' && !item.resolutionQueued) ?? false;
  const feedObjects = allObjects.filter((item) => item.object_type === 'feed_post').sort((a, b) => asString(b.body.created_at).localeCompare(asString(a.body.created_at)));
  const ideaMarks = useMemo(() => new Map(allObjects.filter((item) => item.object_type === 'idea_mark').map((item) => [item.object_id, asString(item.body.state)])), [allObjects]);
  const libraryObjects = allObjects.filter((item) => item.object_type === 'library_item');
  const feedPreference = objectOf<{ content: string }>(allObjects, 'feed_preferences', 'default');
  const preferenceIdeas = useMemo(() => ideasFromFeedPreferences(feedPreference?.body.content ?? ''), [feedPreference?.body.content]);
  const ideaCards = useMemo(() => [
    ...(ideasEnabled ? preferenceIdeas.map((idea) => ({
      id: `preference-${idea.id}`, kind: 'preference' as const, title: idea.title, emoji: idea.emoji,
      body: idea.body, source: '来自你保存的动态偏好', prompt: idea.prompt, ideaID: idea.id,
    })) : []),
    ...(ideasEnabled ? goalSuggestions.map((idea) => ({
      id: `goal-${idea.id}`, kind: 'goal' as const, title: idea.title, emoji: '🎯',
      body: `${idea.stepLabel}：${idea.stepTitle}`, source: '来自你的目标进度', prompt: idea.prompt, ideaID: `goal-${idea.id}`,
    })) : []),
    ...personalIdeas.flatMap((group) => group.ideas.map((idea) => ({
      id: idea.id, kind: 'curated' as const, title: idea.title, emoji: idea.emoji,
      body: idea.body, source: ideaMarks.get(idea.id) === 'saved' ? '已收藏' : ideaMarks.get(idea.id) === 'dismissed' ? '已忽略' : group.title,
      prompt: idea.prompt, ideaID: idea.id, mark: ideaMarks.get(idea.id),
    }))),
  ], [ideasEnabled, preferenceIdeas, goalSuggestions, ideaMarks]);
  const conflicts = useMemo(() => state?.outbox.filter((item) => item.status === 'conflict' && !item.resolutionQueued) ?? [], [state?.outbox]);
  const archivedConflicts = useMemo(() => state?.outbox.filter((item) => item.status === 'conflict' && item.resolutionQueued) ?? [], [state?.outbox]);
  const pendingCount = (state?.outbox.filter((item) => item.status === 'pending').length ?? 0) + (state?.attachments.filter((item) => item.status === 'pending').length ?? 0);
  const remoteLoading = Boolean(state && !state.snapshotComplete && !state.syncError && (typeof navigator === 'undefined' || navigator.onLine));

  const saveObject = useCallback(async (type: PersonalObjectType, id: string, body: PersonalObjectBody): Promise<boolean> => {
    if (!store) return false;
    setActionError(null);
    try {
      const conflict = conflicts.find((entry) => entry.objectType === type && entry.objectID === id);
      if (conflict) await store.rebaseDraft(conflict.id, body);
      else await store.queueReplace(type, id, body);
      void sync();
      return true;
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法保存个人数据。');
      return false;
    }
  }, [store, conflicts, sync]);

  const deleteObject = useCallback(async (type: PersonalObjectType, id: string) => {
    if (!store) return;
    setActionError(null);
    try {
      await store.queueDelete(type, id);
      void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法删除个人数据。');
    }
  }, [store, sync]);

  const addMemory = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const content = memoryContent.trim();
    if (!content) return;
    const ids = new Set(memoryObjects.map((item) => item.object_id));
    const id = randomMemoryID(ids);
    const saved = await saveObject('memory', id, { content, ...(memoryCategory.trim() ? { category: memoryCategory.trim() } : {}) });
    if (saved) {
      setMemoryContent('');
      setMemoryCategory('');
    }
  };

  const addFeedPost = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const title = feedTitle.trim();
    const body = feedBody.trim();
    if (!title || !body) return;
    const saved = await saveObject('feed_post', newIdempotencyKey(), {
      title, type: 'note', created_at: new Date().toISOString(), liked: false, body,
    });
    if (saved) {
      setFeedTitle('');
      setFeedBody('');
    }
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
      if (quota && file.size > quota.remaining_bytes) throw new Error('附件空间不足，请先清理资源库内容。');
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
      void sync();
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
      void sync();
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
      void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法重新排队附件。');
    }
  };

  const importOldMutation = async (sourceScope: string, mutationID: string) => {
    if (!store) return;
    try {
      await store.importOtherInstanceMutation(sourceScope, mutationID);
      await refreshLocal();
      void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法将旧服务草稿复制到当前服务。');
    }
  };

  const importOldAttachment = async (sourceScope: string, attachmentID: string) => {
    if (!store) return;
    try {
      await store.importOtherInstanceAttachment(sourceScope, attachmentID);
      await refreshLocal();
      void sync();
    } catch (error) {
      setActionError(error instanceof Error ? error.message : '无法将旧服务附件复制到当前服务。');
    }
  };

  if (!user) return <section><div className="loading-card" role="status">正在读取账号…</div></section>;

  return <section className="personal-data-page design-panel" data-layout="viewport-fit">
    <div className="design-panel-header">
      <div><h2>我的数据</h2><p>管理个人资料、AI 个性、记忆、动态、灵感和资源库。</p></div>
    </div>
    <div className="connection-section-heading personal-sync-status" aria-live="polite">
      <p className="muted">{syncing ? '正在更新云端数据…' : state?.syncError ? '云端更新待重试' : '保存后自动更新云端'} · {pendingCount} 项待提交 · {conflicts.length} 条冲突草稿</p>
    </div>

    {!serviceInstanceID && <p className="notice notice-warning" role="status">正在确认服务实例；此期间新保存内容只会保存在本机，不会提交到未确认的服务。</p>}

    {localError && <FloatingError className="error-card" role="alert">{localError}<button type="button" className="button button-secondary" onClick={() => void refreshLocal()}>重试</button></FloatingError>}
    {state?.syncError && <FloatingError className="error-card" role="alert">云端更新未完成：{state.syncError} 本地草稿和附件仍保留。<button type="button" className="button button-secondary" disabled={syncing} onClick={() => void sync()}>重试保存</button></FloatingError>}
    {actionError && <FloatingError className="error-inline" role="alert">{actionError}</FloatingError>}
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

    {!!otherInstanceDrafts.length && <section className="notice notice-warning" aria-label="其他服务实例保留的草稿">
      <h2>其他服务实例保留的草稿</h2>
      <p>这些内容来自旧版或其他服务实例，不会自动上传。逐项查看后，你可以选择把副本按当前服务已确认的版本重新排队；旧服务中的删除草稿会要求再次确认。原草稿和附件会保留在旧范围。</p>
      {otherInstanceDrafts.map((candidate) => <details key={candidate.scope}>
        <summary>{candidate.serviceInstanceID ? `服务实例 ${candidate.serviceInstanceID.slice(0, 8)}` : '旧版或尚未确认服务'} · {candidate.state.outbox.length} 条草稿 · {candidate.state.attachments.length} 个附件</summary>
        <ul>{candidate.state.outbox.map((item) => {
          const confirmationKey = `${candidate.scope}\u0000${item.id}`;
          return <li key={item.id}>
            <strong>{item.objectType} / {item.objectID}</strong> · {item.operation === 'delete' ? '删除' : '修改'} · {item.message ?? '已保留'}
            {item.body && <pre>{JSON.stringify(item.body).slice(0, 240)}</pre>}
            {item.resolutionQueued ? <span>已在当前服务另存，原草稿仍保留</span>
              : item.operation === 'delete' ? confirmOldDelete === confirmationKey
                ? <div className="notice notice-warning" role="group" aria-label={`确认在当前服务重试删除 ${item.objectID}`}>
                  <p>这会按当前服务已确认的版本重新执行删除；原服务草稿仍保留。</p>
                  <div className="inline-actions">
                    <button type="button" className="button button-small button-secondary" onClick={() => setConfirmOldDelete(null)}>取消</button>
                    <button type="button" className="button button-small button-danger-quiet" disabled={!state?.snapshotComplete || syncing || !serviceInstanceID}
                      onClick={() => { setConfirmOldDelete(null); void importOldMutation(candidate.scope, item.id); }}>确认并提交删除</button>
                  </div>
                </div>
                : <button type="button" className="button button-small button-secondary" disabled={!state?.snapshotComplete || syncing || !serviceInstanceID}
                  onClick={() => setConfirmOldDelete(confirmationKey)}>在当前服务重试删除</button>
                : <button type="button" className="button button-small button-secondary" disabled={!state?.snapshotComplete || syncing || !serviceInstanceID}
                  onClick={() => void importOldMutation(candidate.scope, item.id)}>复制到当前服务并提交</button>}
          </li>;
        })}{candidate.state.attachments.map((item) => <li key={item.id}>
          <strong>{item.name}</strong> · {formatBytes(item.sizeBytes)} · {item.message ?? '已保留'}
          {item.resolutionQueued ? <span>已在当前服务另存，原附件仍保留</span>
            : <button type="button" className="button button-small button-secondary" disabled={!state?.snapshotComplete || syncing || !serviceInstanceID || Boolean(item.cancelUploadPending)}
              onClick={() => void importOldAttachment(candidate.scope, item.id)}>复制到当前服务并上传</button>}
        </li>)}</ul>
      </details>)}
    </section>}

    {remoteLoading && <div className="loading-card" role="status">正在读取服务端个人数据；本地草稿仍可离线访问。</div>}

    {state && !remoteLoading && activeSection === 'profile' && <section className="connection-section" aria-labelledby="personal-profile-title">
      <div className="connection-section-heading"><div><h2 id="personal-profile-title" className="sr-only">个人资料</h2><p className="muted">这里只保存服务端允许的资料字段。</p></div></div>
      {!profileObject && <p className="empty-card">还没有个人资料。保存后会创建默认资料。</p>}
      <ProfileEditor key={`${profileObject?.revision ?? 0}:${JSON.stringify(profileObject?.body ?? null)}`} value={profileObject} fallbackName={user?.name ?? ''}
        hasConflict={conflicts.some((item) => item.objectType === 'profile' && item.objectID === 'default')}
        onSave={(body) => void saveObject('profile', 'default', body as unknown as PersonalObjectBody)} onInvalid={setActionError} />
    </section>}

    {state && !remoteLoading && activeSection === 'soul' && <section className="connection-section" aria-labelledby="personal-soul-title">
      <div className="connection-section-heading"><div><h2 id="personal-soul-title" className="sr-only">AI 个性</h2><p className="muted">Morde 默认人格由系统提供。这里可以为个人对话设置专属偏好；项目对话不会读取个人 SOUL。删除个人版本会恢复系统默认人格。</p></div></div>
      <SoulEditor key={`${soulObject?.revision ?? 0}:${JSON.stringify(soulObject?.body ?? null)}`} value={soulObject}
        hasConflict={conflicts.some((item) => item.objectType === 'soul' && item.objectID === 'default')}
        onSave={(body) => void saveObject('soul', 'default', body as unknown as PersonalObjectBody)}
        onDelete={() => void deleteObject('soul', 'default')} onInvalid={setActionError} />
    </section>}

    {state && !remoteLoading && activeSection === 'memory' && <section className="connection-section" aria-labelledby="personal-memory-title">
      <div className="connection-section-heading"><div><h2 id="personal-memory-title" className="sr-only">记忆</h2><p className="muted">默认开启后，Morde 可在个人对话中按需检索有限的历史片段和已保存记忆；不会注入完整历史。项目对话不会读取个人记忆。</p></div></div>
      <label className="checkbox-row"><input type="checkbox" checked={memoryEnabled}
        disabled={!state.snapshotComplete || syncing || memorySettingPending}
        onChange={(event) => void saveObject('skill_setting', 'builtin_memory', { enabled: event.target.checked })} />允许 AI 使用个人记忆</label>
      <p className="muted">{memorySettingPending ? '设置正在同步，服务端确认后生效。' : '默认开启；关闭后，个人对话不会读取历史片段或已保存记忆。修改和删除在服务端同步成功后生效。'}</p>
      <form className="form-card form-stack" onSubmit={addMemory}>
        <label>新增记忆<textarea value={memoryContent} maxLength={65536} required onChange={(event) => setMemoryContent(event.target.value)} /></label>
        <label>分类（可选）<input value={memoryCategory} maxLength={64} onChange={(event) => setMemoryCategory(event.target.value)} /></label>
        <button className="button button-primary" type="submit">保存记忆</button>
      </form>
      {memoryObjects.length === 0 ? <p className="empty-card">还没有记忆。</p> : <ScreenPagination
        items={memoryObjects} getKey={(item) => item.object_id} estimatedItemHeight={156} height={300}
        ariaLabel="已保存记忆" className="personal-data-pager"
        renderItem={(item) => <MemoryRow key={`${item.object_id}:${item.revision}:${JSON.stringify(item.body)}`} item={item}
          onSave={(body) => void saveObject('memory', item.object_id, body)} onDelete={() => void deleteObject('memory', item.object_id)} />}
      />}
    </section>}

    {state && !remoteLoading && activeSection === 'feed' && <section className="connection-section" aria-labelledby="personal-feed-title">
      <div className="connection-section-heading"><div><h2 id="personal-feed-title" className="sr-only">动态</h2><p className="muted">个人动态保存在 feed_post 对象中；偏好设置与动态分开保存。</p></div></div>
      <form className="form-card form-stack" onSubmit={addFeedPost}>
        <label>新增动态标题<input value={feedTitle} maxLength={200} required onChange={(event) => setFeedTitle(event.target.value)} /></label>
        <label>内容<textarea value={feedBody} maxLength={65536} required onChange={(event) => setFeedBody(event.target.value)} /></label>
        <button className="button button-primary" type="submit">发布到个人动态</button>
      </form>
      {feedObjects.length === 0 ? <p className="empty-card">还没有动态。</p> : <ScreenPagination
        items={feedObjects} getKey={(item) => item.object_id} estimatedItemHeight={132} height={300}
        ariaLabel="个人动态" className="personal-data-pager"
        renderItem={(item) => {
        const body = item.body as FeedPostBody;
        return <article className="model-connection-card" key={item.object_id}>
          <div className="model-connection-main"><h3>{body.title}</h3><p className="muted">{body.created_at ? new Date(body.created_at).toLocaleString() : '时间未知'} · {body.type}</p><p>{body.body}</p></div>
          <div className="inline-actions"><button type="button" className="button button-secondary" onClick={() => void saveObject('feed_post', item.object_id, { ...body, liked: !body.liked })}>{body.liked ? '取消喜欢' : '喜欢'}</button><button type="button" className="button button-secondary" onClick={() => void deleteObject('feed_post', item.object_id)}>删除</button></div>
        </article>;
      }}
      />}
    </section>}

    {state && !remoteLoading && activeSection === 'preferences' && <section className="connection-section" aria-labelledby="personal-preferences-title">
      <div className="connection-section-heading"><div><h2 id="personal-preferences-title" className="sr-only">偏好</h2><p className="muted">设置你希望关注的动态内容。</p></div></div>
      <FeedPreferencesEditor key={feedPreference?.body.content ?? 'empty'} initialContent={feedPreference?.body.content ?? ''} onSave={saveFeedPreferences} />
    </section>}

    {state && !remoteLoading && activeSection === 'ideas' && <section className="connection-section" aria-labelledby="personal-ideas-title">
      <div className="connection-section-heading"><div><h2 id="personal-ideas-title" className="sr-only">灵感</h2><p className="muted">建议基于当前账号的目标、关注偏好和 Morde 内置目录生成；点选后会预填助手，不会自动提交任务。</p></div></div>
      <section className="personal-data-setting" aria-labelledby="goal-followups-title">
        <h3 id="goal-followups-title">目标受阻自动跟进</h3>
        <label className="checkbox-row"><input type="checkbox" checked={goalFollowupsEnabled}
          disabled={!state.snapshotComplete || syncing || goalFollowupsSettingPending}
          onChange={(event) => void saveObject('skill_setting', 'builtin_goal_followups', { enabled: event.target.checked })} />自动分析新受阻的目标</label>
        <details><summary>查看功能说明</summary><p className="muted">默认关闭。开启后，目标步骤首次变为受阻时，Morde 会创建一个无电脑目标的一次性后台分析任务，使用当前账号的默认模型和只读工具，并给你下一步建议；不会修改目标、创建计划、发送外部消息或操作电脑。执行服务未运行时，任务会排队等待服务启动。关闭后不会创建新的跟进任务。{goalFollowupsSettingPending ? '设置正在同步，服务端确认后生效。' : '修改在服务端确认后生效。'}</p></details>
      </section>
      <label className="checkbox-row"><input type="checkbox" checked={ideasEnabled}
        disabled={!state.snapshotComplete || syncing || ideasSettingPending}
        onChange={(event) => void saveObject('skill_setting', 'builtin_ideas', { enabled: event.target.checked })} />显示个性化建议</label>
      <details className="personal-data-setting-help"><summary>个性化建议说明</summary><p className="muted">只控制灵感页中根据你的目标和关注偏好生成的建议；不会在后台自动创建任务，也不影响已设置的计划和通知。{ideasSettingPending ? '设置正在同步，服务端确认后生效。' : '修改在服务端确认后生效。'}</p></details>
      {ideaCards.length > 0 && <ScreenPagination
        items={ideaCards} getKey={(idea) => idea.id} estimatedItemHeight={112} height={260}
        ariaLabel="灵感建议" className="personal-data-pager personal-ideas-pager"
        renderItem={(idea) => <article className="model-connection-card" key={idea.id}>
          <div className="model-connection-main"><h3>{idea.emoji} {idea.title}</h3><p>{idea.body}</p><small className="muted">{idea.source}</small></div>
          <div className="inline-actions">{idea.kind === 'curated' && <>
            <button type="button" className="button button-secondary" onClick={() => toggleIdea(idea.ideaID)}>{idea.mark === 'saved' ? '取消收藏' : '收藏'}</button>
            {idea.mark !== 'dismissed' && <button type="button" className="button button-secondary" onClick={() => void saveObject('idea_mark', idea.ideaID, { state: 'dismissed' })}>忽略</button>}
          </>}
            <Link className="button button-primary" to="/assistant" state={{ ideaID: idea.ideaID, ideaPrompt: idea.prompt }}>{idea.kind === 'preference' ? '按主题搜索' : idea.kind === 'goal' ? '继续目标' : '带入助手'}</Link>
          </div>
        </article>}
      />}
    </section>}

    {state && !remoteLoading && activeSection === 'library' && <section className="connection-section" aria-labelledby="personal-library-title">
      <div className="connection-section-heading"><div><h2 id="personal-library-title" className="sr-only">资源库</h2><p className="muted">附件字节保存在本机离线队列；联网后按 4 MiB 分块上传。</p></div></div>
      {quota && <p className="notice notice-info" role="status">附件空间已用 {formatBytes(quota.used_bytes)} / {formatBytes(quota.quota_bytes)}，剩余 {formatBytes(quota.remaining_bytes)}；单个附件上限 {formatBytes(quota.max_attachment_bytes)}。</p>}
      {quotaError && <FloatingError className="error-inline" role="alert">无法读取附件空间：{quotaError}<button type="button" className="button button-small button-secondary" onClick={() => void sync()}>重试</button></FloatingError>}
      <form className="form-card form-stack" onSubmit={(event) => event.preventDefault()}>
        <label>资源类型<select value={libraryKind} onChange={(event) => setLibraryKind(event.target.value as LibraryBody['kind'])}><option value="artifact">文档/产物</option><option value="media">媒体</option></select></label>
        <label>上传新资源<input type="file" onChange={(event) => {
          const file = event.currentTarget.files?.[0];
          if (file) void chooseFile(file);
          event.currentTarget.value = '';
        }} /></label>
      </form>
      {libraryObjects.length === 0 ? <p className="empty-card">资源库还是空的。可以先添加本机文件。</p> : <ScreenPagination
        items={libraryObjects} getKey={(item) => item.object_id} estimatedItemHeight={96} height={300}
        ariaLabel="个人资源库" className="personal-data-pager"
        renderItem={(item) => {
        const body = item.body as LibraryBody;
        const attachment = state.attachments.find((entry) => entry.objectID === item.object_id);
        return <article className="model-connection-card" key={item.object_id}>
          <div className="model-connection-main"><h3>{body.name}</h3><p className="muted">{body.kind === 'media' ? '媒体' : '文档/产物'} · {formatBytes(body.size_bytes ?? 0)} · {body.mime ?? '未知格式'}</p>
            {attachment && <p role="status">{attachment.status === 'conflict' ? `附件冲突：${attachment.message ?? '草稿已保留'}` : '附件已保存在本机，等待提交或正在提交'}</p>}</div>
          <div className="inline-actions">{body.attachment_id && <button type="button" className="button button-secondary" onClick={() => void downloadAttachment(item as PersonalObject & { body: LibraryBody })}>下载附件</button>}
            {!body.attachment_id && !attachment && <label className="button button-secondary">添加附件<input className="sr-only" type="file" onChange={(event) => {
              const file = event.currentTarget.files?.[0];
              if (file) void chooseFile(file, item);
              event.currentTarget.value = '';
            }} /></label>}
            <button type="button" className="button button-secondary" onClick={() => void deleteObject('library_item', item.object_id)}>删除</button></div>
        </article>;
      }}
      />}
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

function SoulEditor({ value, hasConflict, onSave, onDelete, onInvalid }: {
  value: PersonalObject | undefined;
  hasConflict: boolean;
  onSave: (body: SoulBody) => void;
  onDelete: () => void;
  onInvalid: (message: string) => void;
}) {
  const [name, setName] = useState(asString(value?.body.name) || 'Morde');
  const [content, setContent] = useState(asString(value?.body.content));
  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const normalizedName = name.trim();
    const normalizedContent = content.trim();
    if (!normalizedName) { onInvalid('人格名称不能为空。'); return; }
    if (!normalizedContent) { onInvalid('人格内容不能为空。'); return; }
    onSave({ name: normalizedName, lang: 'zh-CN', content: normalizedContent });
  };
  return <>
    {!value && <p className="empty-card">当前使用系统默认的 Morde 人格。保存后会为你的个人对话创建覆盖版本。</p>}
    <form className="form-card form-stack" onSubmit={save}>
      <label>人格名称<input value={name} maxLength={80} required onChange={(event) => setName(event.target.value)} /></label>
      <label>SOUL 内容<textarea value={content} maxLength={65536} required onChange={(event) => setContent(event.target.value)} placeholder="写下希望 Morde 长期遵循的交流风格和做事原则" /></label>
      <div className="inline-actions">
        <button type="submit" className="button button-primary">{hasConflict ? '基于远端版本保存' : value ? '保存 AI 个性' : '保存个人人格'}</button>
        {value && <button type="button" className="button button-secondary" onClick={onDelete}>删除个人人格</button>}
      </div>
    </form>
  </>;
}

function MemoryRow({ item, onSave, onDelete }: { item: PersonalObject; onSave: (body: MemoryBody) => void; onDelete: () => void }) {
  const [content, setContent] = useState(asString(item.body.content));
  const [category, setCategory] = useState(asString(item.body.category));
  return <article className="model-connection-card">
    <div className="model-connection-main"><label>记忆内容<textarea value={content} maxLength={65536} onChange={(event) => setContent(event.target.value)} /></label><label>分类<input value={category} maxLength={64} onChange={(event) => setCategory(event.target.value)} /></label></div>
    <div className="inline-actions"><button type="button" className="button button-secondary" onClick={() => onSave({ content: content.trim(), ...(category.trim() ? { category: category.trim() } : {}) })}>保存修改</button><button type="button" className="button button-secondary" onClick={onDelete}>删除</button></div>
  </article>;
}

export default PersonalData;
