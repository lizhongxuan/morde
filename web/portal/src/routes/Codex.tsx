import { useEffect, useMemo, useRef, useState } from 'react';
import { useInfiniteQuery } from '@tanstack/react-query';
import { errorDetails, portalApi } from '../app/api';
import { FloatingError, FloatingNotice } from '../components/FloatingError';

type CodexScope = 'read' | 'live' | 'send' | 'interrupt' | 'approval';
type CodexProfile = {
  profileKey: string; label: string; executableLabel: string; state: string; error?: string | null;
  connectionAuthorized?: boolean; scopes?: CodexScope[]; syncState?: string; syncTotal?: number; syncDone?: number; syncError?: string | null;
};
type CodexMirror = {
  id: string; profileKey: string; threadId: string; title: string; cwdLabel: string; updatedAt: string | null; status: string;
  scopes: CodexScope[]; snapshotState?: string; historyComplete?: boolean; pendingEvents?: number; blockedEvents?: number; snapshotMessage?: string;
};
type CodexThread = { id: string; title: string; cwdLabel: string; updatedAt: string | null; status: string; authorized?: boolean; archived?: boolean };
type CodexState = { authenticated: boolean; codex: { profiles: CodexProfile[]; selectedProfileKey: string | null; sessions: CodexMirror[]; running?: boolean; error?: string | null } | null };
type CodexBridge = {
  codexState?: () => Promise<unknown>;
  codexAddProfile?: () => Promise<unknown>;
  codexSelectProfile?: (profileKey: string) => Promise<unknown>;
  codexListThreads?: (cursor: string | null) => Promise<unknown>;
  codexAuthorizeProfile?: (profileKey: string, scopes: CodexScope[]) => Promise<unknown>;
  codexRevokeProfile?: (profileKey: string) => Promise<unknown>;
  onCodexState?: (listener: (value: unknown) => void) => () => void;
};

const defaultScopes: CodexScope[] = ['read', 'live', 'send'];
const scopeOptions: { id: CodexScope; label: string; detail: string }[] = [
  { id: 'read', label: '读取历史', detail: '同步已有会话消息和可见内容' },
  { id: 'live', label: '同步实时输出', detail: '同步新消息、工具调用、结果和进度' },
  { id: 'send', label: '发送消息', detail: '允许 iOS 向原 Codex 会话发送消息' },
  { id: 'interrupt', label: '中断任务', detail: '允许从 iOS 中断正在运行的任务' },
  { id: 'approval', label: '处理审批', detail: '允许从 iOS 处理 Codex 操作审批' },
];
const eventLabels: Record<string, string> = {
  user_message: '用户消息', assistant_message: 'Codex 回复', assistant_delta: '回复增量', reasoning_summary: '推理摘要', plan: '计划',
  turn_started: '开始执行', turn_completed: '执行完成', turn_failed: '执行失败', turn_cancelled: '已中断', tool_call: '工具调用',
  tool_result: '工具结果', tool_call_summary: '工具调用摘要', tool_result_summary: '工具结果摘要', command_summary: '命令摘要',
  file_change: '文件变化', file_change_summary: '文件变化摘要', child_agent: '子 Agent', subagent_summary: '子 Agent 摘要',
  question: '等待回答', approval_request: '审批请求', approval_status: '审批状态', command_status: '消息发送状态', unsupported: '未支持的事件',
};

function getBridge(): CodexBridge | undefined {
  if (typeof window === 'undefined') return undefined;
  return (window as Window & { morde?: CodexBridge }).morde;
}

function object(value: unknown): Record<string, unknown> | null {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : null;
}

function parseCodexState(value: unknown): CodexState {
  const root = object(value);
  if (!root || typeof root.authenticated !== 'boolean') throw new Error('Codex 连接状态无效');
  if (root.codex === null) return { authenticated: root.authenticated, codex: null };
  const codex = object(root.codex);
  if (!codex || !Array.isArray(codex.profiles) || !Array.isArray(codex.sessions)
    || !(codex.selectedProfileKey === null || typeof codex.selectedProfileKey === 'string')) throw new Error('Codex 配置状态无效');
  const profiles = codex.profiles.map((item) => {
    const profile = object(item);
    if (!profile || typeof profile.profileKey !== 'string' || typeof profile.label !== 'string'
      || typeof profile.executableLabel !== 'string' || typeof profile.state !== 'string') throw new Error('Codex 配置无效');
    return profile as CodexProfile;
  });
  const sessions = codex.sessions.map((item) => {
    const session = object(item);
    if (!session || typeof session.id !== 'string' || typeof session.profileKey !== 'string' || typeof session.threadId !== 'string'
      || typeof session.title !== 'string' || typeof session.cwdLabel !== 'string' || typeof session.status !== 'string' || !Array.isArray(session.scopes)) {
      throw new Error('Codex 会话状态无效');
    }
    return session as CodexMirror;
  });
  return { authenticated: root.authenticated, codex: { profiles, sessions,
    selectedProfileKey: codex.selectedProfileKey as string | null,
    ...(typeof codex.running === 'boolean' ? { running: codex.running } : {}),
    ...(typeof codex.error === 'string' || codex.error === null ? { error: codex.error as string | null } : {}),
  } };
}

function parseThreads(value: unknown): { items: CodexThread[]; nextCursor: string | null } {
  const page = object(value);
  if (!page || !Array.isArray(page.items) || !(page.nextCursor === null || typeof page.nextCursor === 'string')) throw new Error('Codex 会话列表无效');
  return { items: page.items.map((item) => {
    const row = object(item);
    if (!row || typeof row.id !== 'string' || typeof row.title !== 'string' || typeof row.cwdLabel !== 'string' || typeof row.status !== 'string'
      || !(row.updatedAt === null || typeof row.updatedAt === 'string')) throw new Error('Codex 会话列表无效');
    return { id: row.id, title: row.title, cwdLabel: row.cwdLabel, status: row.status, updatedAt: row.updatedAt,
      ...(typeof row.authorized === 'boolean' ? { authorized: row.authorized } : {}),
      ...(typeof row.archived === 'boolean' ? { archived: row.archived } : {}),
    };
  }), nextCursor: page.nextCursor };
}

function scopeList(value: unknown): CodexScope[] {
  if (!Array.isArray(value)) return [];
  return value.filter((item): item is CodexScope => scopeOptions.some((scope) => scope.id === item));
}

function statusLabel(value: string): string {
  const labels: Record<string, string> = {
    starting: '正在连接', authorizing: '等待授权', connected: '已连接', authorized: '已授权', running: '运行中', stopped: '已断开', disconnected: '已断开', offline: '离线', reconnecting: '正在重连', failed: '连接失败', scanning: '正在扫描会话',
    syncing: '正在同步', synced: '同步完成', complete: '同步完成', partial: '部分待同步', pending: '等待同步', in_progress: '执行中', waiting_approval: '等待审批',
    completed: '已完成', failed_turn: '执行失败', idle: '空闲', limited: '部分可读', unknown: '状态未知',
  };
  return labels[value] ?? value;
}

function profileSyncLabel(value?: string): string {
  if (!value) return '正在发现会话';
  if (value === 'synced' || value === 'complete') return '会话发现完成，历史按会话状态继续补齐';
  return '会话发现状态：' + statusLabel(value);
}

function eventBody(payload: unknown): string {
  const value = object(payload);
  if (!value) return typeof payload === 'string' ? payload : JSON.stringify(payload, null, 2);
  for (const key of ['text', 'summary', 'output', 'explanation', 'diagnostic', 'detail', 'items']) {
    if (typeof value[key] === 'string') return value[key] as string;
  }
  return JSON.stringify(value, null, 2);
}

function visibleCodexEvents<T extends { kind: string; turn_id?: string; item_id?: string }>(events: T[]): T[] {
  const completedAssistantItems = new Set(events
    .filter((event) => event.kind === 'assistant_message' && event.turn_id && event.item_id)
    .map((event) => JSON.stringify([event.turn_id, event.item_id])));
  return events.filter((event) => event.kind !== 'assistant_delta' || !event.turn_id || !event.item_id ||
    !completedAssistantItems.has(JSON.stringify([event.turn_id, event.item_id])));
}

type ToolEventDetails = {
  direction: 'input' | 'output';
  tool: string;
  summary: string;
  status: string;
  callID: string;
  partLabel: string;
  chunk: string;
  exitCode?: number;
};

function toolEventDetails(kind: string, payload: unknown, eventStatus?: string): ToolEventDetails | null {
  if (kind !== 'tool_call' && kind !== 'tool_result') return null;
  const value = object(payload) ?? {};
  const input = kind === 'tool_call';
  const part = value[input ? 'input_part' : 'output_part'];
  const parts = value[input ? 'input_parts' : 'output_parts'];
  const partLabel = Number.isSafeInteger(part) && Number(part) >= 0
    ? Number.isSafeInteger(parts) && Number(parts) > Number(part)
      ? `片段 ${Number(part)} / ${Number(parts)}（序号从 0 开始）`
      : `片段 ${Number(part)}（序号从 0 开始；总数待定）`
    : '单个片段或未提供分片信息';
  const rawChunk = value[input ? 'input' : 'output'];
  const chunk = typeof rawChunk === 'string'
    ? rawChunk
    : rawChunk === undefined ? `此事件没有提供完整${input ? '输入' : '输出'}片段。`
      : JSON.stringify(rawChunk, null, 2) ?? String(rawChunk);
  const status = typeof value.status === 'string' ? value.status
    : typeof eventStatus === 'string' ? eventStatus : 'unknown';
  const statusLabels: Record<string, string> = {
    inProgress: '执行中', completed: '已完成', failed: '失败', failedToStart: '启动失败',
    canceled: '已取消', cancelled: '已取消', interrupted: '已中断', notStarted: '未开始',
    in_progress: '执行中', waiting_approval: '等待审批', unknown: '未知',
  };
  return {
    direction: input ? 'input' : 'output',
    tool: typeof value.tool === 'string' && value.tool ? value.tool : 'Codex 工具',
    summary: typeof value.summary === 'string' ? value.summary : '',
    status: Object.hasOwn(statusLabels, status) ? statusLabels[status]! : status,
    callID: typeof value.call_id === 'string' ? value.call_id : '',
    partLabel,
    chunk,
    ...(typeof value.exit_code === 'number' && Number.isFinite(value.exit_code) ? { exitCode: value.exit_code } : {}),
  };
}

export function Codex() {
  const codexBridge = useMemo(() => getBridge(), []);
  const [state, setState] = useState<CodexState | null>(null);
  const [stateError, setStateError] = useState(() => codexBridge?.codexState ? '' : '当前 PC 客户端未提供 Codex 会话桥接。');
  const [message, setMessage] = useState('');
  const [scopes, setScopes] = useState<CodexScope[]>(defaultScopes);
  const [threads, setThreads] = useState<CodexThread[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [threadError, setThreadError] = useState('');
  const [threadLoading, setThreadLoading] = useState(false);
  const [selectedThreadID, setSelectedThreadID] = useState('');
  const loadingProfileRef = useRef('');
  const threadRequestSequenceRef = useRef(0);
  const selectedProfileRef = useRef('');
  const profileKey = state?.codex?.selectedProfileKey ?? '';
  const selectedProfile = state?.codex?.profiles.find((item) => item.profileKey === profileKey);
  const selectedMirror = state?.codex?.sessions.find((item) => item.profileKey === profileKey && item.threadId === selectedThreadID);
  const eventSyncActive = Boolean(selectedMirror && (selectedMirror.historyComplete === false || (selectedMirror.pendingEvents ?? 0) > 0
    || ['in_progress', 'waiting_approval', 'running'].includes(selectedMirror.status)
    || ['pending', 'in_progress', 'running', 'scanning', 'partial'].includes(selectedMirror.snapshotState ?? '')));

  const refreshState = async () => {
    if (!codexBridge?.codexState) return;
    try { setState(parseCodexState(await codexBridge.codexState())); setStateError(''); }
    catch (error) { setStateError(errorDetails(error).message); }
  };

  const loadThreads = async (cursor: string | null) => {
    if (!codexBridge?.codexListThreads || !state?.authenticated || !profileKey || loadingProfileRef.current === profileKey) return;
    const requestedProfile = profileKey;
    const requestSequence = ++threadRequestSequenceRef.current;
    loadingProfileRef.current = requestedProfile; setThreadLoading(true); setThreadError('');
    try {
      const page = parseThreads(await codexBridge.codexListThreads(cursor));
      if (selectedProfileRef.current !== requestedProfile) return;
      setThreads((current) => {
        const combined = cursor ? [...current, ...page.items] : page.items;
        return [...new Map(combined.map((thread) => [thread.id, thread])).values()];
      });
      setNextCursor(page.nextCursor);
      setMessage('已读取 ' + (cursor ? '更多' : '本机') + ' Codex 会话。');
    } catch (error) {
      if (selectedProfileRef.current === requestedProfile) setThreadError(errorDetails(error).message);
    } finally {
      if (threadRequestSequenceRef.current === requestSequence) {
        loadingProfileRef.current = '';
        setThreadLoading(false);
      }
    }
  };

  useEffect(() => {
    let active = true;
    const apply = (value: unknown) => {
      if (!active) return;
      try { setState(parseCodexState(value)); setStateError(''); }
      catch (error) { setStateError(errorDetails(error).message); }
    };
    const stop = codexBridge?.onCodexState?.(apply);
    if (codexBridge?.codexState) void codexBridge.codexState().then(apply).catch((error: unknown) => { if (active) setStateError(errorDetails(error).message); });
    return () => { active = false; stop?.(); };
  }, [codexBridge]);

  useEffect(() => {
    selectedProfileRef.current = profileKey;
    if (!profileKey) {
      threadRequestSequenceRef.current += 1;
      loadingProfileRef.current = '';
      // eslint-disable-next-line react-hooks/set-state-in-effect -- Drop the loading state for the profile that was just deselected.
      setThreadLoading(false);
      setThreads([]); setNextCursor(null); setSelectedThreadID(''); return;
    }
    setThreads([]); setNextCursor(null); setSelectedThreadID('');
    const selected = state?.codex?.profiles.find((item) => item.profileKey === profileKey);
    setScopes(scopeList(selected?.scopes).length ? scopeList(selected?.scopes) : defaultScopes);
    void loadThreads(null);
    // Load only when the selected profile changes, not on every state push.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [profileKey]);

  const selectedEvents = useInfiniteQuery({
    queryKey: ['codex-session-events', selectedMirror?.id],
    queryFn: ({ pageParam }) => portalApi.codexSessionEvents(selectedMirror!.id, pageParam, 100),
    initialPageParam: 0,
    getNextPageParam: (page) => {
      if (!page.next_cursor) return undefined;
      const cursor = Number(page.next_cursor);
      return Number.isSafeInteger(cursor) && cursor > 0 ? cursor : undefined;
    },
    enabled: Boolean(selectedMirror?.id),
    refetchInterval: eventSyncActive ? 2500 : false,
    retry: false,
  });
  const events = selectedEvents.data?.pages.flatMap((page) => page.items).sort((left, right) => left.sequence - right.sequence) ?? [];
  const visibleEvents = visibleCodexEvents(events);
  const eventSequence = selectedEvents.data?.pages.at(-1)?.current_sequence ?? 0;

  const addProfile = async () => {
    if (!codexBridge?.codexAddProfile) return;
    setMessage('选择本机 Codex 配置…');
    try { await codexBridge.codexAddProfile(); await refreshState(); setMessage('Codex 配置已添加。'); }
    catch (error) { setMessage('添加 Codex 配置失败：' + errorDetails(error).message); }
  };
  const chooseProfile = async (value: string) => {
    if (!codexBridge?.codexSelectProfile || !value || value === profileKey) return;
    try { await codexBridge.codexSelectProfile(value); await refreshState(); }
    catch (error) { setMessage('切换 Codex 配置失败：' + errorDetails(error).message); }
  };
  const authorizeProfile = async () => {
    if (!codexBridge?.codexAuthorizeProfile || !profileKey || !selectedProfile || !scopes.includes('read')) return;
    setThreadLoading(true); setMessage('等待确认并开始同步全部会话…');
    try {
      await codexBridge.codexAuthorizeProfile(profileKey, scopes);
      await refreshState();
      await loadThreads(null);
      setMessage('已提交连接授权；本机正在同步该配置下的普通与归档会话。');
    } catch (error) { setMessage('Codex 连接授权未确认：' + errorDetails(error).message); }
    finally { setThreadLoading(false); }
  };
  const revokeProfile = async () => {
    if (!codexBridge?.codexRevokeProfile || !profileKey) return;
    setThreadLoading(true);
    try { await codexBridge.codexRevokeProfile(profileKey); await refreshState(); await loadThreads(null); setMessage('该 Codex 配置的同步授权已撤销。'); }
    catch (error) { setMessage('撤销授权未确认：' + errorDetails(error).message); }
    finally { setThreadLoading(false); }
  };

  return <section className="codex-route">
    <header className="page-heading"><p className="eyebrow">本机 AI</p><h1>Codex 会话</h1><p>连接本机 Codex 配置后，Morde 会同步该配置可访问的普通与归档会话，包括消息、工具调用、结果和 Codex 提供的推理摘要。iOS 可查看这些会话并在授权范围内接续原会话。</p></header>
    {!codexBridge && <p className="notice notice-warning" role="status">Codex 会话同步需要 Morde PC 客户端，请在桌面客户端打开此页面。</p>}
    {stateError && <FloatingError role="alert">{stateError}<button className="button button-secondary button-small" type="button" onClick={() => void refreshState()}>重试</button></FloatingError>}
    {state && !state.authenticated && <FloatingNotice className="notice-warning">请先登录 Morde，再连接本机 Codex。</FloatingNotice>}
    {codexBridge && state?.authenticated && <>
      <section className="design-panel" aria-labelledby="codex-connection-title">
        <div className="design-panel-header"><div><h2 id="codex-connection-title">本机 Codex 配置</h2><p>授权按本地配置生效；Morde 不会读取其他 Codex 配置。</p></div><button type="button" className="button button-secondary" disabled={!codexBridge.codexAddProfile || threadLoading} onClick={() => void addProfile()}>添加配置</button></div>
        {message && <p className="codex-status" role="status" aria-live="polite">{message}</p>}
        {!state.codex?.profiles.length ? <div className="empty-card compact"><p>尚未添加 Codex 配置。添加本机 Codex 配置后即可连接并同步会话。</p></div> : <>
          <label htmlFor="codex-profile-select">本机配置
            <select id="codex-profile-select" value={profileKey} disabled={threadLoading} onChange={(event) => void chooseProfile(event.target.value)}>
              {state.codex.profiles.map((profile) => <option key={profile.profileKey} value={profile.profileKey}>{profile.label} · {profile.executableLabel} · {statusLabel(profile.state)}</option>)}
            </select>
          </label>
          {selectedProfile && <>
            <p className="muted" role="status">本机连接：{statusLabel(selectedProfile.state)}{selectedProfile.error ? ' · ' + selectedProfile.error : ''}</p>
            <div className="codex-scope-list" aria-label="Codex 连接授权范围">
              {scopeOptions.map((option) => <label className="checkbox-row" key={option.id}>
                <input type="checkbox" checked={option.id === 'read' || scopes.includes(option.id)} disabled={option.id === 'read' || threadLoading}
                  onChange={(event) => setScopes((current) => event.target.checked ? [...new Set([...current, option.id])] : current.filter((item) => item !== option.id))} />
                <span><strong>{option.label}</strong><small className="table-sub">{option.detail}</small></span>
              </label>)}
            </div>
            <div className="inline-actions">
              <button type="button" className="button button-primary" disabled={!codexBridge.codexAuthorizeProfile || threadLoading}
                onClick={() => void authorizeProfile()}>{threadLoading ? '正在处理…' : selectedProfile.connectionAuthorized ? '更新同步授权范围' : '连接并同步全部会话'}</button>
              {selectedProfile.connectionAuthorized && <button type="button" className="button button-secondary" disabled={!codexBridge.codexRevokeProfile || threadLoading} onClick={() => void revokeProfile()}>撤销此配置授权</button>}
            </div>
            {selectedProfile.connectionAuthorized && <p className="codex-sync-progress" role="status">
              {profileSyncLabel(selectedProfile.syncState)}
              {Number.isSafeInteger(selectedProfile.syncDone) && Number.isSafeInteger(selectedProfile.syncTotal) ? ' · 已登记 ' + selectedProfile.syncDone + ' / ' + selectedProfile.syncTotal + ' 个会话' : ''}
              {selectedProfile.syncError ? ' · ' + selectedProfile.syncError : ''}
            </p>}
          </>}
        </>}
      </section>
      {selectedProfile && <section className="design-panel" aria-labelledby="codex-threads-title">
        <div className="design-panel-header"><div><h2 id="codex-threads-title">本机 Codex 会话</h2><p>{selectedProfile.connectionAuthorized ? '会话由本机连接持续发现；点击会话可查看已同步事件。' : '连接并授权后，所有可访问会话才会同步到 Morde。'}</p></div>
          <button type="button" className="button button-secondary" disabled={threadLoading || !codexBridge.codexListThreads} onClick={() => void loadThreads(null)}>刷新会话</button>
        </div>
        {threadError && <FloatingError role="alert">{threadError}<button type="button" className="button button-secondary button-small" onClick={() => void loadThreads(null)}>重试</button></FloatingError>}
        {!threads.length && !threadLoading && !threadError && <div className="empty-card compact"><p>{selectedProfile.connectionAuthorized ? '正在等待本机 Codex 会话发现。' : '连接后可查看此配置下的会话。'}</p></div>}
        {!!threads.length && <div className="codex-thread-list">
          {threads.map((thread) => {
            const mirror = state.codex?.sessions.find((item) => item.profileKey === profileKey && item.threadId === thread.id);
            return <button type="button" key={thread.id} className={'codex-thread-row' + (selectedThreadID === thread.id ? ' active' : '')} aria-pressed={selectedThreadID === thread.id} onClick={() => setSelectedThreadID(thread.id)}>
              <span><strong>{thread.title || '未命名会话'}</strong><small>{[thread.cwdLabel, thread.updatedAt ? new Date(thread.updatedAt).toLocaleString() : '', thread.archived ? '归档' : ''].filter(Boolean).join(' · ')}</small></span>
              <span className="status">{mirror ? statusLabel(mirror.status) : thread.authorized ? '等待镜像' : '未同步'}</span>
            </button>;
          })}
        </div>}
        {nextCursor && <button type="button" className="button button-secondary" disabled={threadLoading} onClick={() => void loadThreads(nextCursor)}>{threadLoading ? '正在加载…' : '加载更多会话'}</button>}
        {threadLoading && <p className="muted" role="status">正在读取本机 Codex…</p>}
      </section>}
      {selectedThreadID && <section className="design-panel" aria-labelledby="codex-detail-title">
        <div className="design-panel-header"><div><h2 id="codex-detail-title">{threads.find((thread) => thread.id === selectedThreadID)?.title || 'Codex 会话详情'}</h2><p>{threads.find((thread) => thread.id === selectedThreadID)?.cwdLabel || ''}</p></div></div>
        {!selectedMirror && <p className="muted" role="status">此会话尚未在服务端登记，等待本机完成历史回填。</p>}
        {selectedMirror && <>
          <p className="codex-sync-progress" role="status">{selectedMirror.historyComplete ? '历史已同步' : '历史回填中'} · {statusLabel(selectedMirror.snapshotState ?? selectedMirror.status)} · 已载入 {events.length} 项{eventSequence ? ' / ' + eventSequence : ''}
            {selectedMirror.pendingEvents ? ' · ' + selectedMirror.pendingEvents + ' 项待上传' : ''}{selectedMirror.blockedEvents ? ' · ' + selectedMirror.blockedEvents + ' 项受授权范围限制' : ''}</p>
          {selectedMirror.snapshotMessage && <FloatingNotice className="notice-warning">{selectedMirror.snapshotMessage}</FloatingNotice>}
          {selectedEvents.isError && <FloatingError role="alert">无法读取会话事件：{errorDetails(selectedEvents.error).message}<button type="button" className="button button-secondary button-small" onClick={() => void selectedEvents.refetch()}>重试</button></FloatingError>}
          {selectedEvents.isPending && <FloatingNotice className="notice-info">正在读取同步的 Codex 会话内容…</FloatingNotice>}
          {!selectedEvents.isPending && !events.length && <div className="empty-card compact"><p>该会话当前没有可显示的同步事件。</p></div>}
          {!!visibleEvents.length && <div className="codex-event-list">
            {visibleEvents.map((event) => {
              const tool = toolEventDetails(event.kind, event.payload, event.status);
              if (tool) return <details key={event.sequence} className={'codex-event codex-event-' + event.kind + ' codex-tool-event'}>
                <summary className="codex-event-heading">
                  <span><strong>{tool.direction === 'input' ? '工具调用' : '工具结果'} · {tool.tool}</strong>{tool.summary ? ' · ' + tool.summary : ''}</span>
                  <span>{tool.status} · {tool.partLabel} · {new Date(event.occurred_at).toLocaleString()}</span>
                </summary>
                <div className="codex-tool-event-content">
                  <p><strong>事件：</strong>{tool.direction === 'input' ? '工具调用' : '工具结果'}</p>
                  <p><strong>工具：</strong>{tool.tool}</p>
                  <p><strong>状态：</strong>{tool.status}</p>
                  {tool.callID && <p><strong>调用 ID：</strong><code>{tool.callID}</code></p>}
                  <p><strong>{tool.direction === 'input' ? '输入片段' : '输出片段'}：</strong>{tool.partLabel}</p>
                  {tool.exitCode !== undefined && <p><strong>退出码：</strong>{tool.exitCode}</p>}
                  <pre>{tool.chunk}</pre>
                </div>
              </details>;
              return <article key={event.sequence} className={'codex-event codex-event-' + event.kind}>
                <div className="codex-event-heading"><strong>{eventLabels[event.kind] ?? event.kind}</strong><span>{new Date(event.occurred_at).toLocaleString()}</span></div>
                <pre>{eventBody(event.payload)}</pre>
              </article>;
            })}
          </div>}
          {selectedEvents.hasNextPage && <button type="button" className="button button-secondary" disabled={selectedEvents.isFetchingNextPage} onClick={() => void selectedEvents.fetchNextPage()}>{selectedEvents.isFetchingNextPage ? '正在加载…' : '加载更多历史事件'}</button>}
        </>}
      </section>}
    </>}
  </section>;
}
