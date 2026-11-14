import { useEffect, useRef, useState } from 'react';
import { errorDetails } from '../app/api';
import { formatBytes, formatDate } from '../app/format';

type Mapping = { workspaceId: string; directoryName: string; syncEnabled: boolean; liveRunning: boolean };
type LocalState = { mappings: Mapping[]; syncState: 'idle' | 'running' | 'shutdown_unknown'; nodeState: 'stopped' | 'running' | 'unknown'; safetyCopiesAvailable: boolean; checkpoint: { workspaceId: string; mode: string; scope: string[] } | null };
type Copy = { id: string; workspace_id: string; path: string; size: number; created_at: string };
type Bridge = {
  legacySyncState?: () => Promise<unknown>;
  legacyStopSync?: (id: string) => Promise<unknown>;
  legacyResumeSync?: (id: string) => Promise<unknown>;
  legacyDiscardSync?: (id: string) => Promise<unknown>;
  legacyPauseSync?: (id: string) => Promise<unknown>;
  legacyStopLiveSync?: (id: string) => Promise<unknown>;
  preserved?: (id: string) => Promise<unknown>;
  restorePreserved?: (workspaceId: string, id: string) => Promise<unknown>;
  deletePreserved?: (workspaceId: string, id: string) => Promise<unknown>;
  onState?: (listener: (value: unknown) => void) => () => void;
  onSync?: (listener: (value: unknown) => void) => () => void;
};

function hasControlCharacters(value: string): boolean {
  return [...value].some((character) => {
    const code = character.codePointAt(0) ?? 0;
    return code < 32 || code === 127;
  });
}

function parseState(value: unknown): LocalState {
  if (!value || typeof value !== 'object' || Array.isArray(value)) throw new Error('本机同步状态无效');
  const state = value as Record<string, unknown>;
  if (!Array.isArray(state.mappings) || state.mappings.length > 1000
    || !['idle', 'running', 'shutdown_unknown'].includes(String(state.syncState))
    || !['stopped', 'running', 'unknown'].includes(String(state.nodeState)) || typeof state.safetyCopiesAvailable !== 'boolean') throw new Error('本机同步状态无效');
  const mappings = state.mappings.map((item: unknown) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)) throw new Error('本机目录状态无效');
    const mapping = item as Record<string, unknown>;
    if (typeof mapping.workspaceId !== 'string' || !/^w[a-z0-9]{7}$/.test(mapping.workspaceId)
      || typeof mapping.directoryName !== 'string' || !mapping.directoryName || mapping.directoryName.length > 255
      || /[\\/]/.test(mapping.directoryName) || hasControlCharacters(mapping.directoryName)
      || typeof mapping.syncEnabled !== 'boolean' || typeof mapping.liveRunning !== 'boolean') throw new Error('本机目录状态无效');
    return mapping as Mapping;
  });
  const checkpoint = state.checkpoint;
  if (checkpoint !== null && (!checkpoint || typeof checkpoint !== 'object' || Array.isArray(checkpoint)
    || typeof (checkpoint as Record<string, unknown>).workspaceId !== 'string'
    || !/^w[a-z0-9]{7}$/.test(String((checkpoint as Record<string, unknown>).workspaceId))
    || !['upload', 'download', 'bidirectional', 'live_sync', 'handoff_checkpoint'].includes(String((checkpoint as Record<string, unknown>).mode))
    || !Array.isArray((checkpoint as Record<string, unknown>).scope) || ((checkpoint as { scope: unknown[] }).scope).length > 1000
    || ((checkpoint as { scope: unknown[] }).scope).some((path) => typeof path !== 'string' || !path || path.length > 4096
      || hasControlCharacters(path) || path.startsWith('/') || /^[A-Za-z]:/.test(path) || path.split(/[\\/]/).some((part) => !part || part === '.' || part === '..')))) {
    throw new Error('旧版同步恢复点无效');
  }
  return { mappings, syncState: state.syncState as LocalState['syncState'], nodeState: state.nodeState as LocalState['nodeState'],
    safetyCopiesAvailable: state.safetyCopiesAvailable, checkpoint: checkpoint as LocalState['checkpoint'] };
}

function resumedStatus(value: unknown): string {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return '旧版同步结果未知，恢复点已保留。';
  const result = value as Record<string, unknown>;
  if (Number.isSafeInteger(result.revision) && Number(result.revision) >= 0) return `交接检查点已核验，版本 ${result.revision}。`;
  const session = result.session && typeof result.session === 'object' && !Array.isArray(result.session)
    ? result.session as Record<string, unknown> : {};
  if (session.state === 'completed' && Array.isArray(result.conflicts) && result.conflicts.length === 0) return '旧版同步已完成。';
  if (session.state === 'conflict' || Array.isArray(result.conflicts) && result.conflicts.length) return '旧版同步存在冲突，恢复点已保留。';
  return '旧版同步结果尚未确认，恢复点已保留。';
}

function parseCopies(value: unknown, workspaceId: string): Copy[] {
  if (!Array.isArray(value) || value.length > 10000) throw new Error('本机安全副本列表无效');
  return value.map((item: unknown) => {
    if (!item || typeof item !== 'object' || Array.isArray(item)) throw new Error('本机安全副本无效');
    const copy = item as Record<string, unknown>;
    if (typeof copy.id !== 'string' || !/^[a-f0-9]{32}$/.test(copy.id) || copy.workspace_id !== workspaceId
      || typeof copy.path !== 'string' || !copy.path || copy.path.length > 4096 || copy.path.includes('\\') || hasControlCharacters(copy.path)
      || copy.path.split('/').some((part) => !part || part === '.' || part === '..')
      || typeof copy.created_at !== 'string' || !Number.isFinite(Date.parse(copy.created_at))
      || !Number.isSafeInteger(copy.size) || Number(copy.size) < 0) throw new Error('本机安全副本无效');
    return copy as Copy;
  });
}

export function LocalWorkspaceRecovery({ bridge, workspaceNames }: { bridge: Bridge | undefined; workspaceNames: Record<string, string> }) {
  const [state, setState] = useState<LocalState | null>(null);
  const [error, setError] = useState('');
  const [message, setMessage] = useState('');
  const [busy, setBusy] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [copies, setCopies] = useState<{ workspaceId: string; items: Copy[] } | null>(null);
  const revision = useRef(0);
  const active = useRef(false);

  const refresh = async () => {
    if (!bridge?.legacySyncState) return;
    const request = ++revision.current;
    try {
      const next = parseState(await bridge.legacySyncState());
      if (active.current && request === revision.current) { setState(next); setError(''); }
    } catch {
      if (active.current && request === revision.current) { setState(null); setError('无法读取本机旧版同步状态。'); }
    }
  };
  useEffect(() => {
    active.current = true;
    void Promise.resolve().then(refresh);
    const unsubscribe = bridge?.onState?.(() => void refresh());
    const unsubscribeSync = bridge?.onSync?.(() => void refresh());
    return () => { active.current = false; revision.current += 1; unsubscribe?.(); unsubscribeSync?.(); };
    // The bridge is stable for the lifetime of the desktop page.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bridge]);

  const run = async (operation: () => Promise<unknown>, success: string | ((result: unknown) => string)) => {
    if (busy) return;
    setBusy(true); setError(''); setMessage('');
    try {
      const result = await operation();
      if (!active.current) return;
      if (result && typeof result === 'object' && 'cancelled' in result && result.cancelled === true) setMessage('已取消操作。');
      else if (result === false || result && typeof result === 'object' && 'stopped' in result && result.stopped === false) setMessage('停止状态未确认，请检查本机状态。');
      else setMessage(typeof success === 'function' ? success(result) : success);
      setCopies(null);
    } catch (reason) { if (active.current) setError(errorDetails(reason).message); }
    finally { if (active.current) { await refresh(); setBusy(false); } }
  };
  const stopLegacy = async (id: string) => {
    if (!bridge?.legacyStopSync || stopping) return;
    setStopping(true); setError('');
    try {
      const stopped = await bridge.legacyStopSync(id);
      if (active.current) setMessage(stopped === true ? '同步已停止，恢复点已保留。' : '停止状态未确认，请检查本机状态。');
    } catch (reason) { if (active.current) setError(errorDetails(reason).message); }
    finally { if (active.current) { await refresh(); setStopping(false); } }
  };
  const loadCopies = async (id: string) => {
    if (!bridge?.preserved || busy) return;
    setBusy(true); setError(''); setMessage('');
    try {
      const items = parseCopies(await bridge.preserved(id), id);
      if (active.current) setCopies({ workspaceId: id, items });
    } catch (reason) { if (active.current) setError(`无法读取本机安全副本：${errorDetails(reason).message}`); }
    finally { if (active.current) setBusy(false); }
  };
  if (!bridge?.legacySyncState || !state && !error || state && !state.mappings.length && !state.checkpoint && !error) return null;

  return <details className="local-workspace-recovery">
    <summary>已有 Workspace 本机同步与安全副本</summary>
    <p className="muted">这里保留旧版同步的停止、恢复点和本机安全副本操作。</p>
    {error && <p role="alert">{error} <button type="button" className="button button-secondary button-small" disabled={busy} onClick={() => void refresh()}>重试</button></p>}
    {message && <p role="status">{message}</p>}
    {state?.checkpoint && <div>
      <p>旧版同步恢复点：{workspaceNames[state.checkpoint.workspaceId] ?? state.checkpoint.workspaceId} · {state.checkpoint.mode}</p>
      <p className="muted">同步范围：{state.checkpoint.scope.length ? state.checkpoint.scope.join('、') : '整个目录'}</p>
      <div className="inline-actions">
        {state.syncState === 'running' && <button type="button" className="button button-secondary button-small" disabled={stopping || !bridge.legacyStopSync}
          onClick={() => void stopLegacy(state.checkpoint!.workspaceId)}>{stopping ? '正在停止…' : '停止旧版同步'}</button>}
        {state.syncState !== 'running' && <>
          <button type="button" className="button button-secondary button-small" disabled={busy || !bridge.legacyResumeSync || state.syncState !== 'idle' || state.nodeState === 'unknown'}
            onClick={() => void run(() => bridge.legacyResumeSync!(state.checkpoint!.workspaceId), resumedStatus)}>按原模式继续同步</button>
          <button type="button" className="button button-secondary button-small" disabled={busy || !bridge.legacyDiscardSync}
            onClick={() => void run(() => bridge.legacyDiscardSync!(state.checkpoint!.workspaceId), '已放弃旧版同步恢复点。')}>放弃旧版恢复点</button>
        </>}
      </div>
    </div>}
    {state?.mappings.map((mapping) => <div className="local-recovery-row" key={mapping.workspaceId}>
      <strong>{workspaceNames[mapping.workspaceId] ?? mapping.workspaceId}</strong> · {mapping.directoryName}
      <div className="inline-actions">
        {mapping.liveRunning && <button type="button" className="button button-secondary button-small" disabled={busy || !bridge.legacyStopLiveSync}
          onClick={() => void run(() => bridge.legacyStopLiveSync!(mapping.workspaceId), '旧版实时同步已停止。')}>停止旧版实时同步</button>}
        {mapping.syncEnabled && <button type="button" className="button button-secondary button-small" disabled={busy || !bridge.legacyPauseSync}
          onClick={() => void run(() => bridge.legacyPauseSync!(mapping.workspaceId), '本机同步已暂停。')}>暂停旧版本机同步</button>}
        <button type="button" className="button button-secondary button-small" disabled={busy || !bridge.preserved || !state.safetyCopiesAvailable}
          onClick={() => void loadCopies(mapping.workspaceId)}>查看本机安全副本</button>
      </div>
      {copies?.workspaceId === mapping.workspaceId && <div>
        {!copies.items.length && <p className="muted">暂无本机安全副本。</p>}
        {copies.items.map((copy) => <div className="local-recovery-copy" key={copy.id}>
          <span>{copy.path} · {formatBytes(copy.size)} · {formatDate(copy.created_at)}</span>
          <div className="inline-actions">
            <button type="button" className="button button-secondary button-small" disabled={busy || !bridge.restorePreserved || !state.safetyCopiesAvailable} onClick={() => void run(() => bridge.restorePreserved!(mapping.workspaceId, copy.id), '已恢复到本机，安全副本仍保留。')}>恢复到本机</button>
            <button type="button" className="button button-secondary button-small" disabled={busy || !bridge.deletePreserved || !state.safetyCopiesAvailable} onClick={() => void run(() => bridge.deletePreserved!(mapping.workspaceId, copy.id), '已删除本机安全副本。')}>删除副本</button>
          </div>
        </div>)}
      </div>}
    </div>)}
    {state && !state.safetyCopiesAvailable && <p className="muted">请先停止本机同步并处理恢复点，再查看或恢复安全副本。</p>}
  </details>;
}
