import { useCallback, useEffect, useMemo, useRef, useState, type PointerEvent as ReactPointerEvent } from 'react';
import {
  gatewayApi,
  type GatewayControl,
  type GatewayControlAction,
  type GatewayControlRequest,
  type GatewayHost,
  type GatewayTerminalSession,
} from '../api/gateway';
import { ApiError } from '../api/client';
import FileManager from '../components/FileManager';
import TopBar from '../components/TopBar';
import TaskObservation from '../components/TaskObservation';
import WorkspaceBrowser from '../components/WorkspaceBrowser';
import FloatingError from '../components/FloatingError';
import { returnToPreviousPage } from './navigation';
import { isTransientNetworkError } from './aio';
import { useWorkbenchNotice } from './notice';

type Tab = 'browser' | 'terminal' | 'workspace' | 'files' | 'vscode';
type EmbedTarget = 'browser' | 'terminal' | 'files' | 'vscode';
type MobileMenu = 'browser' | 'terminal' | 'files' | null;
type EmbedContext = { origin: string; nonce: string };
type NativeWorkbenchBridge = {
  hostWorkbenchReady?: (nonce: string) => Promise<unknown>;
  sendHostWorkbenchState?: (state: unknown) => Promise<unknown>;
  onHostWorkbenchMessage?: (listener: (value: unknown) => void) => () => void;
};
const embedChannel = 'morde.workbench.embed';
const embedProtocolVersion = 1;
const primaryTabs: { id: Tab; label: string }[] = [
  { id: 'browser', label: '桌面' },
  { id: 'terminal', label: '终端' },
  { id: 'workspace', label: 'Workspace' },
  { id: 'files', label: '主机文件' },
  { id: 'vscode', label: 'VS Code' },
];
const tabIDs = new Set(primaryTabs.map((tab) => tab.id));
const embedTargetIDs = new Set<EmbedTarget>(['browser', 'terminal', 'files', 'vscode']);

function requestedEmbedTab(search: string): Tab | null {
  const values = new URLSearchParams(search).getAll('tab');
  return values.length === 1 && tabIDs.has(values[0] as Tab) ? values[0] as Tab : null;
}

function authorizedEmbedTargets(host: GatewayHost | null): Set<Tab> {
  return new Set((host?.embed_targets ?? []).filter((target): target is Tab => tabIDs.has(target as Tab)));
}

function embedTabAllowed(targets: Set<Tab>, tab: Tab): boolean {
  return targets.has(tab) || (tab === 'files' && targets.has('workspace')) || (tab === 'workspace' && targets.has('files'));
}

function embedTargetForTab(tab: Tab, targets: Set<Tab>): EmbedTarget | null {
  if (tab === 'workspace' || tab === 'files') return embedTabAllowed(targets, tab) ? 'files' : null;
  return embedTargetIDs.has(tab as EmbedTarget) && targets.has(tab) ? tab as EmbedTarget : null;
}

function visibleEmbeddedTab(active: Tab, initial: Tab | null, targets: Set<Tab>): Tab | null {
  if (embedTabAllowed(targets, active)) return active;
  return initial && embedTabAllowed(targets, initial) ? initial : null;
}

function initialEmbedTab(targets: Set<Tab>, requested: Tab | null): Tab | null {
  if (requested && embedTabAllowed(targets, requested)) return requested;
  return (['browser', 'terminal', 'files', 'workspace', 'vscode'] as Tab[])
    .find((tab) => embedTabAllowed(targets, tab)) ?? null;
}

function hasEmbedControlTarget(targets: Set<Tab>): boolean {
  return targets.has('browser');
}

function parseEmbedContext(search: string, referrer: string, native = false): EmbedContext | null {
  const query = new URLSearchParams(search);
  const nonce = query.get('embed_nonce') ?? '';
  if (query.get('embed') !== '1' || !/^[\w-]{24,128}$/.test(nonce)) return null;
  if (native) return { origin: '', nonce };
  try {
    const origin = new URL(referrer).origin;
    if (!origin || (new URL(origin).protocol !== 'https:' && !/^http:\/\/(localhost|127\.0\.0\.1)(:\d+)?$/.test(origin))) return null;
    return { origin, nonce };
  } catch {
    return null;
  }
}

function embedMessageMatches(value: unknown, context: EmbedContext): value is Record<string, unknown> {
  if (!value || typeof value !== 'object') return false;
  const message = value as Record<string, unknown>;
  return message.channel === embedChannel && message.version === embedProtocolVersion && message.nonce === context.nonce;
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : '请求失败，请稍后重试';
}

function requestId() {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

async function confirmFrameResponse(url: string, frame: HTMLIFrameElement, readySelector: string, label: string) {
  const appLabel = label === 'VS Code' ? `${label} ` : label;
  let response: Response;
  try {
    response = await fetch(url, { credentials: 'same-origin', cache: 'no-store' });
  } catch {
    throw new Error(`${appLabel}无法连接云端电脑服务`);
  }
  if (!response.ok) throw new Error(`${appLabel}页面请求失败（HTTP ${response.status}）`);
  // shortcut: AIO and code-server expose no readiness API; update these selectors when the pinned pages change.
  const deadline = Date.now() + 10_000;
  while (Date.now() < deadline) {
    try {
      if (frame.contentDocument?.querySelector(readySelector)) return;
    } catch {
      // A cross-origin or not-yet-attached frame is not ready.
    }
    await new Promise((resolve) => window.setTimeout(resolve, 250));
  }
  throw new Error(`${appLabel}页面已响应，但 10 秒内界面未就绪`);
}

function controlActionCompleted(control: GatewayControl, action: GatewayControlAction) {
  const leaseMatches = action === 'take_over'
    ? control.control_state === 'user' && control.control_lease_active && control.control_lease_held_by_me
    : control.control_state === 'ai' && !control.control_lease_active;
  return leaseMatches && !control.pending_operation_ids?.length &&
    (!control.active_transition || control.active_transition.phase === 'completed');
}

function controlTransitionPending(control: GatewayControl | null) {
  return !!control && (control.control_state === 'taking_over' || control.control_state === 'returning' ||
    !!control.pending_operation_ids?.length || !!control.active_transition && control.active_transition.phase !== 'completed');
}

function pendingControlAction(control: GatewayControl | null): GatewayControlAction | null {
  return control?.active_transition?.action ?? (control?.control_state === 'taking_over'
    ? 'take_over'
    : control?.control_state === 'returning' ? 'release' : null);
}

export default function Workbench() {
  const query = new URLSearchParams(window.location.search);
  const [nativeBridge] = useState(() => {
    const bridge = (window as Window & { morde?: NativeWorkbenchBridge }).morde;
    return bridge?.hostWorkbenchReady && bridge.sendHostWorkbenchState && bridge.onHostWorkbenchMessage ? bridge : null;
  });
  const [embedContext] = useState(() => parseEmbedContext(window.location.search, document.referrer, Boolean(nativeBridge)));
  const embedded = embedContext !== null;
  const taskObservationOnly = query.has('task_id');
  const taskObservationID = query.get('task_id') ?? '';
  const workspaceOnly = !embedded && query.get('tab') === 'workspace';
  const hostSessionOnly = workspaceOnly || taskObservationOnly;
  const [host, setHost] = useState<GatewayHost | null>(null);
  const embedTargets = useMemo(() => authorizedEmbedTargets(host), [host]);
  const requestedTab = requestedEmbedTab(window.location.search);
  const initialTab = initialEmbedTab(embedTargets, requestedTab);
  const hostID = host?.id ?? null;
  const [control, setControl] = useState<GatewayControl | null>(null);
  const controlRef = useRef<GatewayControl | null>(null);
  const [controlLoading, setControlLoading] = useState(!hostSessionOnly && !embedded);
  const [controlError, setControlError] = useState('');
  const [controlNetworkUnavailable, setControlNetworkUnavailable] = useState(false);
  const [controlRetry, setControlRetry] = useState(0);
  const [controlActionError, setControlActionError] = useState('');
  const [controlActionNetworkUnavailable, setControlActionNetworkUnavailable] = useState(false);
  const [controlActionPending, setControlActionPending] = useState(false);
  const [controlRequest, setControlRequest] = useState<GatewayControlRequest | null>(null);
  const [pageError, setPageError] = useState('');
  const [pageRetry, setPageRetry] = useState(0);
  const [embedVisible, setEmbedVisible] = useState(!embedded);
  const [embedConversationID, setEmbedConversationID] = useState<string | null>(null);
  const [active, setActive] = useState<Tab>(() => hostSessionOnly ? 'workspace' : requestedTab ?? 'browser');
  const [controlLocked, setControlLocked] = useState(embedded);
  const [selectionSeq, setSelectionSeq] = useState<number | null>(null);
  const [lockSeq, setLockSeq] = useState<number | null>(null);
  const [filePanel, setFilePanel] = useState<'files' | 'workspace'>(() => requestedTab === 'workspace' ? 'workspace' : 'files');
  const [loaded, setLoaded] = useState<Set<Tab>>(() => new Set(hostSessionOnly ? ['workspace'] : embedded ? [] : ['browser']));
  const loadedTabs = useMemo(() => initialTab ? new Set([...loaded, initialTab]) : loaded, [initialTab, loaded]);
  const [browserStatus, setBrowserStatus] = useState<'connecting' | 'connected' | 'disconnected' | 'failed'>('connecting');
  const [browserReconnect, setBrowserReconnect] = useState(0);
  const [browserFullscreen, setBrowserFullscreen] = useState(false);
  const [showWindowHelp, setShowWindowHelp] = useState(false);
  const [mobileMenu, setMobileMenu] = useState<MobileMenu>(null);
  const [terminalSession, setTerminalSession] = useState<GatewayTerminalSession | null>(null);
  const [terminalError, setTerminalError] = useState('');
  const [terminalRetry, setTerminalRetry] = useState(0);
  const [terminalFrameStatus, setTerminalFrameStatus] = useState<'loading' | 'loaded' | 'failed'>('loading');
  const [terminalFrameError, setTerminalFrameError] = useState('');
  const [terminalFrameRetry, setTerminalFrameRetry] = useState(0);
  const [terminalCreationUncertain, setTerminalCreationUncertain] = useState(false);
  const [terminalRequestPending, setTerminalRequestPending] = useState(false);
  const [vscodeFrameStatus, setVscodeFrameStatus] = useState<'loading' | 'loaded' | 'failed'>('loading');
  const [vscodeFrameError, setVscodeFrameError] = useState('');
  const [vscodeFrameRetry, setVscodeFrameRetry] = useState(0);
  const [renewing, setRenewing] = useState(false);
  const [notice, setNotice] = useState('');
  const [noticeKind, setNoticeKind] = useState<'success' | 'error'>('success');
  const notifyNetwork = useWorkbenchNotice();
  const browserFrameRef = useRef<HTMLIFrameElement>(null);
  const terminalFrameRef = useRef<HTMLIFrameElement>(null);
  const vscodeFrameRef = useRef<HTMLIFrameElement>(null);
  const browserPanelRef = useRef<HTMLDivElement>(null);
  const workbenchTabsRef = useRef<HTMLElement>(null);
  const browserObserverRef = useRef<MutationObserver | null>(null);
  const browserConnectTimeoutRef = useRef<number | undefined>(undefined);
  const browserConnectedRef = useRef(false);
  const controlRequestRef = useRef<GatewayControlRequest | null>(null);
  const controlLockedRef = useRef(embedded);
  const selectionSeqRef = useRef(-1);
  const selectionCommandSeqRef = useRef(-1);
  const lockSeqRef = useRef(-1);
  const activeRef = useRef<Tab>(hostSessionOnly ? 'workspace' : requestedTab ?? 'browser');
  const embedInitializedRef = useRef(false);
  const embedVisibleRef = useRef(!embedded);
  const leaseRenewalRef = useRef(false);
  const terminalRequestRef = useRef(false);
  const terminalReplacementRef = useRef(false);
  const mobileLongPressTimerRef = useRef<number | undefined>(undefined);
  const suppressTabClickRef = useRef(false);
  const suppressTabClickTimerRef = useRef<number | undefined>(undefined);

  const applyControl = useCallback((value: GatewayControl) => {
    controlRef.current = value;
    setControl(value);
    setControlError('');
    setControlNetworkUnavailable(false);
    setControlActionNetworkUnavailable(false);
    if (value.control_state !== 'user') {
      setTerminalSession(null);
      setTerminalError('');
    }
    const pending = controlRequestRef.current;
    if (pending && controlActionCompleted(value, pending.action)) {
      controlRequestRef.current = null;
      setControlRequest(null);
      setControlActionError('');
    }
  }, []);

  function setEmbeddedControlLocked(locked: boolean) {
    controlLockedRef.current = locked;
    setControlLocked(locked);
  }

  const handleControlReadFailure = useCallback((error: unknown) => {
    setControlError(messageOf(error));
    const transient = isTransientNetworkError(error);
    setControlNetworkUnavailable(transient);
    if (transient) notifyNetwork('电脑控制状态连接失败，操作已暂停。', {
      label: '重试', onClick: () => setControlRetry((value) => value + 1),
    });
  }, [notifyNetwork]);

  const reloadControl = useCallback(async () => {
    if (embedded && !hasEmbedControlTarget(embedTargets)) return;
    setControlLoading(true);
    try {
      applyControl(await gatewayApi.control());
    } catch (error) {
      handleControlReadFailure(error);
    } finally {
      setControlLoading(false);
    }
  }, [applyControl, embedTargets, embedded, handleControlReadFailure]);

  const selectEmbeddedTab = useCallback(async (tab: Tab, sequence?: number, forceFileTab = false) => {
    if (!embedded) return;
    if (sequence !== undefined) {
      if (!Number.isSafeInteger(sequence) || sequence < 0 || sequence <= selectionCommandSeqRef.current) return;
      selectionCommandSeqRef.current = sequence;
    }
    const target = embedTargetForTab(tab, embedTargets);
    if (!target || !hasEmbedControlTarget(embedTargets)) return;

    let currentControl: GatewayControl;
    try {
      currentControl = await gatewayApi.control();
      applyControl(currentControl);
    } catch (error) {
      handleControlReadFailure(error);
      return;
    }
    if ((sequence !== undefined && sequence !== selectionCommandSeqRef.current) || controlLockedRef.current ||
      controlActionPending || !controlActionCompleted(currentControl, 'take_over')) return;

    const nextTab = target === 'files'
      ? tab === 'workspace' && embedTargets.has('workspace')
        ? 'workspace'
        : !forceFileTab && tab === 'files' && activeRef.current === 'workspace' && embedTargets.has('workspace')
          ? 'workspace'
          : embedTargets.has('files') ? 'files' : 'workspace'
      : tab;
    activeRef.current = nextTab;
    setActive(nextTab);
    if (sequence !== undefined) {
      selectionSeqRef.current = sequence;
      setSelectionSeq(sequence);
    }
    if (target === 'files') setFilePanel(nextTab === 'workspace' ? 'workspace' : 'files');
    setLoaded((previous) => new Set(previous).add(nextTab));
  }, [applyControl, controlActionPending, embedTargets, embedded, handleControlReadFailure]);

  useEffect(() => {
    if (!embedContext) return;
    const onMessage = (message: Record<string, unknown>) => {
      if (!embedMessageMatches(message, embedContext)) return;
      if (message.type === 'init' && typeof message.visible === 'boolean' &&
        typeof message.conversation_id === 'string' && /^[\w-]{8,128}$/.test(message.conversation_id)) {
        embedInitializedRef.current = true;
        setEmbedConversationID(message.conversation_id);
        embedVisibleRef.current = message.visible;
        setEmbedVisible(message.visible);
        if (message.visible) void reloadControl();
      } else if (message.type === 'visibility' && embedInitializedRef.current && typeof message.visible === 'boolean') {
        const wasVisible = embedVisibleRef.current;
        embedVisibleRef.current = message.visible;
        setEmbedVisible(message.visible);
        if (message.visible && !wasVisible) void reloadControl();
      } else if (embedInitializedRef.current && message.type === 'select_target' &&
        typeof message.target === 'string' && embedTargetIDs.has(message.target as EmbedTarget) &&
        typeof message.seq === 'number') {
        const target = message.target as EmbedTarget;
        void selectEmbeddedTab(target === 'files' && embedTargets.has('files') &&
          activeRef.current === 'workspace' && embedTargets.has('workspace') ? 'workspace' : target, message.seq);
      } else if (embedInitializedRef.current && message.type === 'control_lock' &&
        typeof message.locked === 'boolean' && typeof message.seq === 'number' &&
        Number.isSafeInteger(message.seq) && message.seq >= 0 && message.seq > lockSeqRef.current) {
        lockSeqRef.current = message.seq;
        setLockSeq(message.seq);
        if (message.locked) {
          setEmbeddedControlLocked(true);
        } else if (!hasEmbedControlTarget(embedTargets)) {
          setEmbeddedControlLocked(true);
        } else {
          const requestedSeq = message.seq;
          void gatewayApi.control().then((value) => {
            applyControl(value);
            if (lockSeqRef.current === requestedSeq && value.control_state === 'user' &&
              value.control_lease_active && value.control_lease_held_by_me) {
              setEmbeddedControlLocked(false);
            } else if (lockSeqRef.current === requestedSeq) setEmbeddedControlLocked(true);
          }).catch((error: unknown) => {
            if (lockSeqRef.current === requestedSeq) setEmbeddedControlLocked(true);
            handleControlReadFailure(error);
          });
        }
      }
    };
    if (nativeBridge) {
      const unsubscribe = nativeBridge.onHostWorkbenchMessage?.((value) => {
        if (!value || typeof value !== 'object') return;
        onMessage(value as Record<string, unknown>);
      });
      void Promise.resolve(nativeBridge.hostWorkbenchReady?.(embedContext.nonce)).catch(() => undefined);
      return unsubscribe;
    }
    const onWindowMessage = (event: MessageEvent<unknown>) => {
      if (event.source !== window.parent || event.origin !== embedContext.origin || !event.data || typeof event.data !== 'object') return;
      onMessage(event.data as Record<string, unknown>);
    };
    window.addEventListener('message', onWindowMessage);
    window.parent.postMessage({ channel: embedChannel, version: embedProtocolVersion, type: 'ready', nonce: embedContext.nonce }, embedContext.origin);
    return () => window.removeEventListener('message', onWindowMessage);
  }, [applyControl, embedContext, embedTargets, handleControlReadFailure, nativeBridge, reloadControl, selectEmbeddedTab]);

  useEffect(() => {
    if (!embedContext || !embedInitializedRef.current || !embedConversationID || !hostID) return;
    const message = {
      channel: embedChannel,
      version: embedProtocolVersion,
      type: 'state',
      nonce: embedContext.nonce,
      conversation_id: embedConversationID,
      host_id: hostID,
      workbench_ready: !pageError && !controlError && (hostSessionOnly || (hostID !== null && !controlLoading && (!embedded || initialTab !== null))),
      remote_desktop: browserStatus,
      control_state: controlError ? 'unknown' : control?.control_state ?? 'unknown',
      control_lease_active: controlError ? false : control?.control_lease_active ?? false,
      control_lease_held_by_me: controlError ? false : control?.control_lease_held_by_me ?? false,
      active_target: embedded
        ? (() => {
          const visibleTab = visibleEmbeddedTab(active, initialTab, embedTargets);
          const target = visibleTab ? embedTargetForTab(visibleTab, embedTargets) : null;
          return target === 'files' && !hasEmbedControlTarget(embedTargets) ? null : target;
        })()
        : null,
      control_version: controlError ? null : control?.control_version ?? null,
      control_locked: controlLocked,
      selection_seq: selectionSeq,
      lock_seq: lockSeq,
    };
    if (nativeBridge) void Promise.resolve(nativeBridge.sendHostWorkbenchState?.(message)).catch(() => undefined);
    else window.parent.postMessage(message, embedContext.origin);
  }, [browserStatus, control?.control_lease_active, control?.control_lease_held_by_me, control?.control_state,
    control?.control_version, controlError, controlLoading, controlLocked, embedContext, embedConversationID, embedded,
    embedTargets, hostID, hostSessionOnly, initialTab, lockSeq, nativeBridge, pageError, selectionSeq, active]);

  useEffect(() => {
    if (hostSessionOnly) return;
    let current = true;
    const loadControl = () => {
      setControlLoading(true);
      void gatewayApi.control()
        .then((value) => current && applyControl(value))
        .catch((error: unknown) => current && handleControlReadFailure(error))
        .finally(() => current && setControlLoading(false));
    };
    if (!embedded) loadControl();
    gatewayApi.host()
      .then((value) => {
        if (!current) return;
        setHost(value);
        if (embedded) {
          const targets = authorizedEmbedTargets(value);
          if (hasEmbedControlTarget(targets)) loadControl();
          else {
            setControl(null);
            setControlError('');
            setControlLoading(false);
          }
        }
      })
      .catch((error: unknown) => {
        if (!current) return;
        if (isTransientNetworkError(error)) {
          notifyNetwork('工作台主机状态连接失败，正在保留当前页面。', {
            label: '重试', onClick: () => setPageRetry((attempt) => attempt + 1),
          });
        } else setPageError(messageOf(error));
      });
    return () => {
      current = false;
    };
  }, [applyControl, embedded, handleControlReadFailure, hostSessionOnly, notifyNetwork, pageRetry, controlRetry]);

  const controlSessionOnly = hostSessionOnly || (embedded && !hasEmbedControlTarget(embedTargets));

  useEffect(() => {
    if (controlSessionOnly) return;
    const interval = controlActionPending || controlTransitionPending(control) || controlRequest ? 1000 : 3000;
    let current = true;
    const refresh = async () => {
      try {
        const value = await gatewayApi.control();
        if (current) applyControl(value);
      } catch (error) {
        if (current) handleControlReadFailure(error);
      }
    };
    const timer = window.setInterval(() => void refresh(), interval);
    return () => {
      current = false;
      window.clearInterval(timer);
    };
  }, [applyControl, control, controlActionPending, controlRequest, controlSessionOnly, handleControlReadFailure]);

  useEffect(() => {
    if (controlSessionOnly || !control?.control_lease_active || !control.control_lease_held_by_me) return;
    const timer = window.setInterval(() => {
      if (leaseRenewalRef.current) return;
      leaseRenewalRef.current = true;
      void gatewayApi.renewControlLease(control.control_version)
        .then(applyControl)
        .catch(async (error: unknown) => {
          if (isTransientNetworkError(error)) {
            setControlActionNetworkUnavailable(true);
            notifyNetwork('操作租约续期失败，正在确认电脑控制状态。');
          } else setControlActionError(`操作租约续期失败：${messageOf(error)}`);
          try { applyControl(await gatewayApi.control()); }
          catch (statusError) { handleControlReadFailure(statusError); }
        })
        .finally(() => { leaseRenewalRef.current = false; });
    }, 10_000);
    return () => window.clearInterval(timer);
  }, [applyControl, control?.control_lease_active, control?.control_lease_held_by_me, control?.control_version, controlSessionOnly, handleControlReadFailure, notifyNetwork]);

  const stopBrowserWatch = useCallback(() => {
    browserObserverRef.current?.disconnect();
    browserObserverRef.current = null;
    if (browserConnectTimeoutRef.current !== undefined) {
      window.clearTimeout(browserConnectTimeoutRef.current);
      browserConnectTimeoutRef.current = undefined;
    }
  }, []);

  useEffect(() => () => {
    stopBrowserWatch();
    if (mobileLongPressTimerRef.current !== undefined) window.clearTimeout(mobileLongPressTimerRef.current);
    if (suppressTabClickTimerRef.current !== undefined) window.clearTimeout(suppressTabClickTimerRef.current);
  }, [stopBrowserWatch]);

  useEffect(() => {
    const onFullscreenChange = () => setBrowserFullscreen(document.fullscreenElement === browserPanelRef.current);
    document.addEventListener('fullscreenchange', onFullscreenChange);
    return () => document.removeEventListener('fullscreenchange', onFullscreenChange);
  }, []);

  useEffect(() => {
    if (!mobileMenu) return;
    const dismissOnOutsidePress = (event: PointerEvent) => {
      if (!workbenchTabsRef.current?.contains(event.target as Node)) setMobileMenu(null);
    };
    document.addEventListener('pointerdown', dismissOnOutsidePress, true);
    return () => document.removeEventListener('pointerdown', dismissOnOutsidePress, true);
  }, [mobileMenu]);

  const leaseHeldByMe = !!control && controlActionCompleted(control, 'take_over');
  const transitionPending = controlTransitionPending(control);
  const transitionAction = pendingControlAction(control);
  const terminalAvailable = !controlLoading && !controlError && !controlActionPending && leaseHeldByMe && (!embedded || !controlLocked);
  const vscodeAvailable = !controlLoading && !controlError && leaseHeldByMe && !controlActionPending && (!embedded || !controlLocked);

  useEffect(() => {
    if (terminalSession?.state !== 'ready' || terminalFrameStatus !== 'loading') return;
    const timer = window.setTimeout(() => {
      setTerminalFrameError('15 秒内没有收到终端页面加载事件');
      setTerminalFrameStatus('failed');
    }, 15_000);
    return () => window.clearTimeout(timer);
  }, [terminalFrameStatus, terminalSession?.state, terminalSession?.url, terminalFrameRetry]);

  useEffect(() => {
    if (!loadedTabs.has('vscode') || !vscodeAvailable || vscodeFrameStatus !== 'loading') return;
    const timer = window.setTimeout(() => {
      setVscodeFrameError('15 秒内没有收到 VS Code 页面加载事件');
      setVscodeFrameStatus('failed');
    }, 15_000);
    return () => window.clearTimeout(timer);
  }, [loadedTabs, vscodeAvailable, vscodeFrameStatus, vscodeFrameRetry]);

  useEffect(() => {
    if (active !== 'terminal' || !terminalAvailable || terminalSession || terminalError || terminalCreationUncertain || terminalRequestRef.current || terminalReplacementRef.current) return;
    let current = true;
    terminalRequestRef.current = true;
    setTerminalRequestPending(true);
    gatewayApi.ensureTerminalSession()
      .then((session) => {
        if (!current) return;
        if (session.state === 'ready') {
          const url = new URL(session.url, window.location.origin);
          if (url.origin !== window.location.origin || url.pathname !== '/terminal') {
            throw new Error('服务器返回了无效的终端地址');
          }
        }
        setTerminalSession(session);
        if (session.state === 'ready') {
          setTerminalFrameError('');
          setTerminalFrameStatus('loading');
        }
      })
      .catch((error: unknown) => {
        if (!current) return;
        if (isTransientNetworkError(error)) {
          notifyNetwork('终端状态读取失败，当前页面状态已保留。', {
            label: '重试', onClick: () => setTerminalRetry((value) => value + 1),
          });
        } else setTerminalError(messageOf(error));
      })
      .finally(() => {
        if (current) {
          terminalRequestRef.current = false;
          setTerminalRequestPending(false);
        }
      });
    return () => {
      current = false;
      terminalRequestRef.current = false;
    };
  }, [active, terminalAvailable, terminalSession, terminalError, terminalCreationUncertain, terminalRetry, notifyNetwork]);

  useEffect(() => {
    if (active !== 'terminal' || !terminalAvailable || terminalSession?.state !== 'creating' || terminalError) return;
    let current = true;
    let timer: number | undefined;
    const poll = async () => {
      if (!current) return;
      if (terminalRequestRef.current) {
        timer = window.setTimeout(() => void poll(), 1000);
        return;
      }
      terminalRequestRef.current = true;
      let stillCreating = false;
      try {
        const session = await gatewayApi.ensureTerminalSession();
        if (!current) return;
        if (session.state === 'ready') {
          const url = new URL(session.url, window.location.origin);
          if (url.origin !== window.location.origin || url.pathname !== '/terminal') {
            throw new Error('服务器返回了无效的终端地址');
          }
        }
        stillCreating = session.state === 'creating';
        setTerminalSession(session);
        if (session.state === 'ready') {
          setTerminalFrameError('');
          setTerminalFrameStatus('loading');
        }
      } catch (error) {
        if (!current) return;
        if (isTransientNetworkError(error)) {
          notifyNetwork('终端状态读取失败，保留当前终端状态。', {
            label: '重试', onClick: () => setTerminalRetry((value) => value + 1),
          });
        } else setTerminalError(messageOf(error));
      } finally {
        terminalRequestRef.current = false;
        if (current && stillCreating) timer = window.setTimeout(() => void poll(), 2000);
      }
    };
    timer = window.setTimeout(() => void poll(), 2000);
    return () => {
      current = false;
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [active, terminalAvailable, terminalError, terminalSession?.state, terminalRetry, notifyNetwork]);

  async function createNewTerminalSession() {
    if (terminalRequestRef.current || !window.confirm('新终端不会停止原终端中的命令。请先保存工作；原会话可能继续运行并占用资源。确定新建吗？')) return;
    terminalReplacementRef.current = true;
    terminalRequestRef.current = true;
    setTerminalError('');
    setTerminalRequestPending(true);
    try {
      const session = await gatewayApi.newTerminalSession();
      if (session.state === 'ready') {
        const url = new URL(session.url, window.location.origin);
        if (url.origin !== window.location.origin || url.pathname !== '/terminal') {
          throw new Error('服务器返回了无效的终端地址');
        }
      }
      setTerminalSession(session);
      if (session.state === 'ready') {
        setTerminalFrameError('');
        setTerminalFrameStatus('loading');
      }
    } catch (error) {
      if (isTransientNetworkError(error)) {
        setTerminalCreationUncertain(true);
        notifyNetwork('新建终端请求未确认，可能已创建。为避免重复创建，暂时保留原终端并暂停新建。');
      } else setTerminalError(messageOf(error));
    } finally {
      terminalReplacementRef.current = false;
      terminalRequestRef.current = false;
      setTerminalRequestPending(false);
    }
  }

  function selectTab(tab: Tab) {
    if (embedded && !embedTargets.has(tab)) return;
    if (tab === 'vscode' && !vscodeAvailable) return;
    activeRef.current = tab;
    setActive(tab);
    setLoaded((previous) => new Set(previous).add(tab));
  }

  function cancelMobileLongPress() {
    if (mobileLongPressTimerRef.current !== undefined) {
      window.clearTimeout(mobileLongPressTimerRef.current);
      mobileLongPressTimerRef.current = undefined;
    }
  }

  function suppressNextTabClick() {
    suppressTabClickRef.current = true;
    if (suppressTabClickTimerRef.current !== undefined) window.clearTimeout(suppressTabClickTimerRef.current);
    suppressTabClickTimerRef.current = window.setTimeout(() => {
      suppressTabClickRef.current = false;
      suppressTabClickTimerRef.current = undefined;
    }, 800);
  }

  function clearSuppressedTabClick() {
    suppressTabClickRef.current = false;
    if (suppressTabClickTimerRef.current !== undefined) {
      window.clearTimeout(suppressTabClickTimerRef.current);
      suppressTabClickTimerRef.current = undefined;
    }
  }

  function beginMobileLongPress(menu: Exclude<MobileMenu, null>) {
    return (event: ReactPointerEvent<HTMLButtonElement>) => {
      if (event.pointerType !== 'touch') return;
      cancelMobileLongPress();
      mobileLongPressTimerRef.current = window.setTimeout(() => {
        mobileLongPressTimerRef.current = undefined;
        suppressNextTabClick();
        setMobileMenu(menu);
      }, 450);
    };
  }

  function openMobileMenuFromContext(event: React.MouseEvent<HTMLButtonElement>, menu: Exclude<MobileMenu, null>) {
    if (!window.matchMedia?.('(max-width: 760px)').matches) return;
    event.preventDefault();
    cancelMobileLongPress();
    suppressNextTabClick();
    setMobileMenu(menu);
  }

  function handleTabClick(tab: Tab) {
    if (embedded && (tab === 'files'
      ? !embedTargets.has('files') && !embedTargets.has('workspace')
      : !embedTargets.has(tab))) return;
    if (suppressTabClickRef.current) {
      clearSuppressedTabClick();
      return;
    }
    if (embedded && tab === 'files') {
      setMobileMenu((current) => current === 'files' ? null : 'files');
      return;
    }
    setMobileMenu(null);
    selectTab(tab);
  }

  function runMobileMenuAction(action: () => void) {
    setMobileMenu(null);
    action();
  }

  function reconnectBrowser() {
    stopBrowserWatch();
    browserConnectedRef.current = false;
    setBrowserStatus('connecting');
    setBrowserReconnect((current) => current + 1);
  }

  function browserFrameLoaded() {
    stopBrowserWatch();
    browserConnectedRef.current = false;
    setBrowserStatus('connecting');

    const document = browserFrameRef.current?.contentDocument;
    const root = document?.documentElement;
    if (!root) {
      browserConnectTimeoutRef.current = window.setTimeout(() => setBrowserStatus('failed'), 15_000);
      return;
    }

    const syncStatus = () => {
      if (root.classList.contains('noVNC_connected')) {
        browserConnectedRef.current = true;
        setBrowserStatus('connected');
        if (browserConnectTimeoutRef.current !== undefined) {
          window.clearTimeout(browserConnectTimeoutRef.current);
          browserConnectTimeoutRef.current = undefined;
        }
      } else if (root.classList.contains('noVNC_connecting') || root.classList.contains('noVNC_reconnecting')) {
        setBrowserStatus('connecting');
      } else if (browserConnectedRef.current) {
        setBrowserStatus('disconnected');
      }
    };

    browserObserverRef.current = new MutationObserver(syncStatus);
    browserObserverRef.current.observe(root, { attributes: true, attributeFilter: ['class'] });
    syncStatus();
    if (!browserConnectedRef.current) {
      browserConnectTimeoutRef.current = window.setTimeout(() => {
        browserConnectTimeoutRef.current = undefined;
        setBrowserStatus((current) => current === 'connecting' ? 'failed' : current);
      }, 15_000);
    }
  }

  function fitBrowser() {
    setNotice('');
    try {
      const frameWindow = browserFrameRef.current?.contentWindow;
      if (!frameWindow) throw new Error('远程桌面尚未加载');
      const resizeMode = browserFrameRef.current?.contentDocument?.getElementById('noVNC_setting_resize') as HTMLSelectElement | null;
      if (resizeMode && frameWindow) {
        resizeMode.value = 'scale';
        const changeEvent = frameWindow.document.createEvent('Event');
        changeEvent.initEvent('change', true, false);
        resizeMode.dispatchEvent(changeEvent);
      }
      if (frameWindow) {
        const resizeEvent = frameWindow.document.createEvent('Event');
        resizeEvent.initEvent('resize', false, false);
        frameWindow.dispatchEvent(resizeEvent);
      }
    } catch {
      setNoticeKind('error');
      setNotice('无法调整桌面画面，请使用 noVNC 控制栏中的缩放设置');
    }
  }

  function restoreWindow() {
    setNotice('');
    try {
      if (browserStatus === 'connected') {
        const document = browserFrameRef.current?.contentDocument;
        const altKey = document?.getElementById('noVNC_toggle_alt_button');
        const tabKey = document?.getElementById('noVNC_send_tab_button');
        if (altKey && tabKey) {
          const altAlreadyDown = altKey.classList.contains('noVNC_selected');
          if (!altAlreadyDown) altKey.click();
          try {
            tabKey.click();
          } finally {
            if (!altAlreadyDown) altKey.click();
          }
        }
      }
    } catch {
      setNoticeKind('error');
      setNotice('无法发送窗口切换快捷键，请点击远程桌面后按 Alt+Tab');
    }
    setShowWindowHelp(true);
  }

  function openMobileKeyboard() {
    if (!leaseHeldByMe || browserStatus !== 'connected') return;
    try {
      const button = browserFrameRef.current?.contentDocument?.getElementById('noVNC_keyboard_button');
      if (!button) throw new Error('keyboard unavailable');
      button.click();
    } catch {
      setNoticeKind('error');
      setNotice('远程键盘暂时不可用，请使用画面中的 noVNC 键盘按钮');
    }
  }

  async function toggleBrowserFullscreen() {
    setNotice('');
    try {
      if (document.fullscreenElement === browserPanelRef.current) {
        await document.exitFullscreen();
      } else {
        await browserPanelRef.current?.requestFullscreen();
      }
    } catch {
      setNoticeKind('error');
      setNotice('桌面画面无法进入全屏，请检查浏览器的全屏权限');
    }
  }

  async function renew() {
    setRenewing(true);
    setNotice('');
    setNoticeKind('success');
    try {
      setHost(await gatewayApi.renew());
      setNotice('主机已续期');
    } catch (error) {
      if (isTransientNetworkError(error)) {
        notifyNetwork('主机续期结果未确认，请稍后核对主机状态。');
      } else {
        setNoticeKind('error');
        setNotice(messageOf(error));
      }
    } finally {
      setRenewing(false);
    }
  }

  async function controlComputer() {
    if (!control) return;
    const transition = control.active_transition;
    const retryTransition = transition?.failure_reason ? transition : null;
    const current = controlRequestRef.current;
    if (!current && !retryTransition && control.control_state === 'user' && !leaseHeldByMe) return;
    const action = current?.action ?? retryTransition?.action ?? (leaseHeldByMe ? 'release' : 'take_over');
    const request = current ?? {
      action,
      request_id: retryTransition?.request_id ?? requestId(),
      expected_control_version: retryTransition?.expected_control_version ?? control.control_version,
    };
    if (request.action === 'release') {
      setActive('browser');
      setTerminalSession(null);
      setTerminalError('');
    }
    controlRequestRef.current = request;
    setControlRequest(request);
    setControlActionPending(true);
    setControlActionError('');
    setControlActionNetworkUnavailable(false);
    try {
      applyControl(await gatewayApi.controlAction(request));
    } catch (error) {
      if (isTransientNetworkError(error)) {
        setControlActionNetworkUnavailable(true);
        notifyNetwork('电脑控制请求未确认，当前操作已暂停。', { label: '重试', onClick: () => void controlComputer() });
      } else setControlActionError(messageOf(error));
      try {
        const latest = await gatewayApi.control();
        applyControl(latest);
        if (error instanceof ApiError && error.status === 409 && controlRequestRef.current === request &&
          latest.control_version !== request.expected_control_version && latest.active_transition?.request_id !== request.request_id) {
          controlRequestRef.current = null;
          setControlRequest(null);
        }
      } catch (statusError) {
        handleControlReadFailure(statusError);
      }
    } finally {
      setControlActionPending(false);
    }
  }

  const failedTransition = control?.active_transition?.failure_reason ? control.active_transition : null;
  const vscodeURL = '/code-server/?folder=%2Fhome%2Fgem%2Fworkspace';
  const browserViewOnly = controlLoading || !!controlError || !leaseHeldByMe || (embedded && controlLocked);
  const retryControlRequest = controlRequest ?? (failedTransition ? {
    action: failedTransition.action,
    request_id: failedTransition.request_id,
    expected_control_version: failedTransition.expected_control_version,
  } : null);
  const controlActionLabel = controlError ? '' : controlActionPending && controlRequest
    ? controlRequest.action === 'take_over' ? '接管中…' : '交回中…'
    : (controlActionError || failedTransition) && retryControlRequest
      ? retryControlRequest.action === 'take_over' ? '重试接管' : '重试交回 AI'
      : transitionPending ? transitionAction === 'take_over' ? '接管中…'
        : transitionAction === 'release' ? '交回中…' : '正在确认控制状态…'
      : control?.control_state === 'ai' ? '接管电脑'
        : leaseHeldByMe ? '交回 AI' : '';
  const mobileControlStatus = controlError && !controlNetworkUnavailable ? '控制不可用'
    : controlNetworkUnavailable || (controlLoading && !control) ? '正在连接'
      : transitionPending ? transitionAction === 'take_over' ? '正在接管'
        : transitionAction === 'release' ? '正在交还' : '正在确认控制状态'
      : control?.control_state === 'ai' ? 'AI 控制中'
        : leaseHeldByMe ? '你正在操作'
          : control?.control_state === 'user' ? '他人正在操作' : '控制状态未知';
  const authorizedActive = embedded && !embedTabAllowed(embedTargets, active) ? initialTab ?? active : active;
  const visibleActive = authorizedActive === 'vscode' && !vscodeAvailable && !embedded ? 'browser' : authorizedActive;
  const visibleTabs = workspaceOnly ? primaryTabs.filter((tab) => tab.id === 'workspace')
    : embedded ? host ? primaryTabs.filter((tab) => tab.id !== 'workspace' && (tab.id === 'files'
      ? embedTabAllowed(embedTargets, 'files')
      : embedTargets.has(tab.id))) : [] : primaryTabs;
  const showEmbeddedFileSwitch = embedded && embedTargets.has('files') && embedTargets.has('workspace');
  const embeddedFileSwitch = () => showEmbeddedFileSwitch ? (
    <div className="embedded-file-switch" role="group" aria-label="文件视图">
      <button type="button" aria-pressed={filePanel === 'files'} disabled={!vscodeAvailable}
        onClick={() => void selectEmbeddedTab('files', undefined, true)}>主机文件</button>
      <button type="button" aria-pressed={filePanel === 'workspace'} disabled={!vscodeAvailable}
        onClick={() => void selectEmbeddedTab('workspace', undefined, true)}>Workspace</button>
    </div>
  ) : null;
  if (taskObservationOnly) return <TaskObservation key={taskObservationID} taskID={taskObservationID} />;
  return (
    <main className={`workbench-app${workspaceOnly ? ' is-workspace-only' : ''}${embedded ? ' is-embedded' : ''}${embedded && !embedVisible ? ' is-embedded-hidden' : ''}`}
      aria-hidden={embedded && !embedVisible ? true : undefined}
      inert={embedded && !embedVisible}>
      {!workspaceOnly && !embedded && <TopBar
        embedded={embedded}
        host={host}
        renewing={renewing}
        onRenew={renew}
        control={controlNetworkUnavailable ? null : control}
        controlLoading={controlLoading || controlNetworkUnavailable}
        controlError={controlNetworkUnavailable ? '' : controlError}
        controlActionError={controlActionNetworkUnavailable ? '' : controlActionError}
        controlActionLabel={controlActionLabel}
        controlActionPending={controlActionPending}
        notice={notice}
        noticeKind={noticeKind}
        onControlAction={controlComputer}
        onRetryControl={reloadControl}
      />}
      {embedded && !host && !pageError && <p className="panel-loading" role="status">正在确认工作台权限…</p>}
      {embedded && host && !embedTargets.size && !pageError && <p className="panel-loading" role="status">此会话没有可用的电脑功能。</p>}
      {pageError ? (
        <section className="page-message page-error" role="alert">
          <span className="page-error-mark" aria-hidden="true">!</span>
          <h1>工作台暂时无法加载</h1>
          <p>服务或工作电脑暂时无法连接。请返回上一页，或重试当前页面。</p>
          <div className="page-error-actions">
            <button className="button button-secondary" type="button" onClick={returnToPreviousPage}>返回上一页</button>
            <button className="button" type="button" onClick={() => {
              setPageError('');
              setPageRetry((attempt) => attempt + 1);
            }}>重试</button>
          </div>
        </section>
      ) : (
        <>
          <div className="workbench-content">
          {!embedded && <nav ref={workbenchTabsRef} className="workbench-tabs" aria-label="工作台功能">
            <div className="primary-tabs">
              {visibleTabs.map((tab) => {
                const menu: MobileMenu = tab.id === 'browser' || tab.id === 'terminal' || tab.id === 'files' ? tab.id : null;
                return (
                <button
                  key={tab.id}
                  className={`tab-button${active === tab.id || (tab.id === 'files' && visibleActive === 'workspace') ? ' is-active' : ''}`}
                  data-tab={tab.id}
                  type="button"
                  aria-current={visibleActive === tab.id || (tab.id === 'files' && visibleActive === 'workspace') ? 'page' : undefined}
                  disabled={tab.id === 'vscode' && !vscodeAvailable}
                  title={embedded ? tab.label : tab.id === 'vscode' && !vscodeAvailable ? '请先接管电脑' : undefined}
                  aria-label={tab.id === 'browser' ? '浏览器' : tab.id === 'files' ? '文件' : tab.label}
                  aria-description={tab.id === 'browser' ? '长按打开窗口操作' : tab.id === 'terminal' ? '长按打开终端操作' : tab.id === 'files' ? '长按打开更多功能' : undefined}
                  aria-haspopup={tab.id === 'browser' || tab.id === 'terminal' || tab.id === 'files' ? 'menu' : undefined}
                  aria-expanded={menu !== null && mobileMenu === menu}
                  onClick={() => handleTabClick(tab.id)}
                  onPointerDown={menu ? beginMobileLongPress(menu) : undefined}
                  onPointerUp={cancelMobileLongPress}
                  onPointerCancel={cancelMobileLongPress}
                  onPointerLeave={cancelMobileLongPress}
                  onContextMenu={menu ? (event) => openMobileMenuFromContext(event, menu) : undefined}
                  onKeyDown={menu ? (event) => {
                    if (event.key === 'ContextMenu' || (event.shiftKey && event.key === 'F10')) {
                      setMobileMenu(menu);
                      event.preventDefault();
                    }
                  } : undefined}
                >
                  <span className={`tab-icon tab-icon-${tab.id}`} aria-hidden="true">
                    {tab.id === 'browser' ? (
                      <svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="8.5" /><path d="M3.5 12h17M12 3.5c2.4 2.3 3.6 5.2 3.6 8.5s-1.2 6.2-3.6 8.5c-2.4-2.3-3.6-5.2-3.6-8.5s1.2-6.2 3.6-8.5Z" /></svg>
                    ) : tab.id === 'terminal' ? (
                      <svg viewBox="0 0 24 24"><rect x="3" y="4.5" width="18" height="15" rx="2.5" /><path d="m7 9.5 3 2.5-3 2.5M12.5 15.5H17" /></svg>
                    ) : tab.id === 'files' ? (
                      <svg viewBox="0 0 24 24"><path d="M3.5 6.5A1.5 1.5 0 0 1 5 5h4l2 2.5h8A1.5 1.5 0 0 1 20.5 9v8.5a1.5 1.5 0 0 1-1.5 1.5H5a1.5 1.5 0 0 1-1.5-1.5Z" /></svg>
                    ) : tab.id === 'vscode' ? (
                      <svg viewBox="0 0 24 24"><path d="M8.5 8.5 4.5 12l4 3.5M15.5 8.5l4 3.5-4 3.5M13.5 5.5l-3 13" /></svg>
                    ) : null}
                  </span>
                  <span className="tab-label-desktop">{tab.label}</span>
                  <span className="tab-label-mobile" aria-hidden="true">{tab.id === 'browser' ? '浏览器' : tab.id === 'files' ? '文件' : tab.label}</span>
                </button>
                );
              })}
            </div>
            {visibleActive === 'browser' && (
              <div className="browser-actions" aria-label="桌面画面控制">
                <span className="browser-connection" data-state={browserStatus} role="status">
                  {browserStatus === 'connected' ? '远程桌面已连接'
                    : browserStatus === 'connecting' ? '正在连接远程桌面…'
                      : browserStatus === 'disconnected' ? '远程桌面已断开' : '远程桌面连接失败'}
                </span>
                <button className="button button-secondary" type="button" onClick={fitBrowser}>
                  适应窗口
                </button>
                <button className="button button-secondary mobile-browser-action" type="button" disabled={browserViewOnly || browserStatus !== 'connected'} onClick={openMobileKeyboard}>
                  键盘
                </button>
                <button className="button button-secondary mobile-browser-action" type="button" disabled={browserViewOnly || browserStatus !== 'connected'} onClick={restoreWindow}>
                  Alt+Tab
                </button>
                <button className="button button-secondary" type="button" onClick={restoreWindow}>
                  找回窗口
                </button>
                <button className="button button-secondary" type="button" onClick={toggleBrowserFullscreen}>
                  {browserFullscreen ? '退出全屏' : '全屏'}
                </button>
              </div>
            )}
            {mobileMenu && !workspaceOnly && (
              <div className="mobile-context-menu" role="menu" aria-label={mobileMenu === 'browser' ? '窗口操作' : mobileMenu === 'terminal' ? '终端操作' : embedded ? '文件视图' : '更多功能'} onKeyDown={(event) => {
                if (event.key === 'Escape') setMobileMenu(null);
              }}>
                {mobileMenu === 'browser' ? (
                  <>
                    <button type="button" role="menuitem" disabled={browserViewOnly || browserStatus !== 'connected'} onClick={() => runMobileMenuAction(openMobileKeyboard)}>键盘</button>
                    <button type="button" role="menuitem" onClick={() => runMobileMenuAction(fitBrowser)}>适应窗口</button>
                    <button type="button" role="menuitem" onClick={() => runMobileMenuAction(restoreWindow)}>Alt+Tab</button>
                    <button type="button" role="menuitem" onClick={() => runMobileMenuAction(restoreWindow)}>找回窗口</button>
                    <button type="button" role="menuitem" onClick={() => runMobileMenuAction(() => void toggleBrowserFullscreen())}>{browserFullscreen ? '退出全屏' : '全屏'}</button>
                  </>
                ) : mobileMenu === 'terminal' ? (
                  <button type="button" role="menuitem" disabled={!terminalAvailable || terminalRequestPending} onClick={() => runMobileMenuAction(() => void createNewTerminalSession())}>新建终端</button>
                ) : (
                  <>
                    {(!embedded || embedTargets.has('workspace')) && <button type="button" role="menuitem" onClick={() => runMobileMenuAction(() => selectTab('workspace'))}>Workspace</button>}
                    {(!embedded || embedTargets.has('files')) && <button type="button" role="menuitem" onClick={() => runMobileMenuAction(() => selectTab('files'))}>主机文件</button>}
                    {!embedded && <button type="button" role="menuitem" disabled={!vscodeAvailable} onClick={() => runMobileMenuAction(() => selectTab('vscode'))}>VS Code</button>}
                  </>
                )}
              </div>
            )}
          </nav>}
            {loadedTabs.has('browser') && (
              <div ref={browserPanelRef} className="tab-panel tab-panel-browser" data-connection-state={browserStatus}
                aria-hidden={visibleActive !== 'browser'} inert={visibleActive !== 'browser'}>
                {browserStatus !== 'connected' && (
                  <div className="connection-overlay" role={browserStatus === 'failed' || browserStatus === 'disconnected' ? 'alert' : 'status'}>
                    {browserStatus === 'connecting' && embedded && <span className="connection-spinner" aria-hidden="true" />}
                    <p>{embedded
                      ? browserStatus === 'connecting' ? '正在连接远程桌面…'
                        : browserStatus === 'disconnected' ? '远程桌面连接已断开。'
                          : '远程桌面连接失败。'
                      : <>
                        <span className="connection-message-desktop">{browserStatus === 'failed' ? '远程桌面没有返回连接状态，请确认主机正在运行后重试。'
                          : browserStatus === 'disconnected' ? '远程桌面连接已中断，可以重新连接当前主机。'
                            : '正在连接远程桌面…'}</span>
                        <span className="connection-message-mobile">{browserStatus === 'failed' ? '连接失败'
                          : browserStatus === 'disconnected' ? '连接已断开'
                            : '正在连接…'}</span>
                      </>}
                    </p>
                    {!embedded && (browserStatus === 'failed' || browserStatus === 'disconnected') && (
                      <button className="button button-secondary" type="button" onClick={reconnectBrowser}>
                        重新连接
                      </button>
                    )}
                  </div>
                )}
                <iframe
                  key={browserReconnect}
                  ref={browserFrameRef}
                  title="远程桌面"
                  src={`/vnc/index.html?autoconnect=true&reconnect=true&resize=scale&view_only=${browserViewOnly || (embedded && !embedVisible)}`}
                  onLoad={browserFrameLoaded}
                />
                {showWindowHelp && (
                  <div className="window-help-backdrop" role="presentation" onClick={() => setShowWindowHelp(false)}>
                    <section className="window-help" role="dialog" aria-modal="true" aria-labelledby="window-help-title" onClick={(event) => event.stopPropagation()}>
                      <h2 id="window-help-title">找回被遮挡的窗口</h2>
                      <p>已尝试发送 <kbd>Alt</kbd> + <kbd>Tab</kbd> 切换应用。若仍未找到，点击桌面后继续按此快捷键；Linux 桌面也可按 <kbd>Super</kbd> 键查看窗口。</p>
                      <p className="muted">如果键盘没有响应，请确认当前主机允许手动操作，再点击桌面后重试。</p>
                      <button className="button" type="button" onClick={() => setShowWindowHelp(false)}>知道了</button>
                    </section>
                  </div>
                )}
              </div>
            )}
            {loadedTabs.has('terminal') && (
              <div className="tab-panel tab-panel-terminal" aria-hidden={visibleActive !== 'terminal'} inert={visibleActive !== 'terminal'}>
                {terminalError ? (
                  <FloatingError>
                    {terminalError}
                    <button className="button button-secondary" type="button" onClick={() => setTerminalError('')}>
                      重新连接
                    </button>
                  </FloatingError>
                ) : terminalAvailable && terminalSession?.state === 'ready' ? (
                  <>
                    {visibleActive === 'terminal' && (
                      <div className="terminal-actions">
                        <button className="button button-secondary" type="button" disabled={terminalRequestPending || terminalCreationUncertain} onClick={() => void createNewTerminalSession()}>
                          新建终端
                        </button>
                      </div>
                    )}
                    <iframe
                      key={`${terminalSession.url}:${terminalFrameRetry}`}
                      ref={terminalFrameRef}
                      title="主机终端"
                      src={terminalSession.url}
                      hidden={visibleActive !== 'terminal'}
                      onLoad={(event) => {
                        const frame = event.currentTarget;
                        void confirmFrameResponse(terminalSession.url, frame, '[aria-label="Terminal input"]', '终端')
                          .then(() => {
                            if (terminalFrameRef.current !== frame) return;
                            setTerminalFrameError('');
                            setTerminalFrameStatus('loaded');
                          })
                          .catch((error: unknown) => {
                            if (terminalFrameRef.current !== frame) return;
                            setTerminalFrameError(messageOf(error));
                            setTerminalFrameStatus('failed');
                          });
                      }}
                      onError={(event) => {
                        if (terminalFrameRef.current !== event.currentTarget) return;
                        setTerminalFrameError('终端页面加载失败');
                        setTerminalFrameStatus('failed');
                      }}
                    />
                    {terminalFrameStatus !== 'loaded' && (
                      <div className="connection-overlay" role={terminalFrameStatus === 'failed' ? 'alert' : 'status'}>
                        {terminalFrameStatus === 'loading' ? <>
                          <span className="connection-spinner" aria-hidden="true" />
                          <p>正在连接终端…</p>
                        </> : <>
                          <p>{terminalFrameError || '终端连接失败，请稍后重试。'}</p>
                          <button className="button button-secondary" type="button" onClick={() => {
                            setTerminalFrameError('');
                            setTerminalFrameStatus('loading');
                            setTerminalFrameRetry((value) => value + 1);
                          }}>重新连接终端</button>
                        </>}
                      </div>
                    )}
                  </>
                ) : terminalAvailable && (terminalSession?.state === 'unknown' || terminalSession?.state === 'interrupted') ? (
                  <FloatingError>
                    {terminalSession.state === 'unknown'
                      ? '原终端的创建结果不确定，可能仍有命令在运行。系统不会自动创建第二个终端。'
                      : '原终端会话已中断或无法恢复。若原命令可能仍在运行，请先核实状态；系统不会自动创建第二个终端。'}
                    <button className="button button-secondary" type="button" disabled={terminalRequestPending} onClick={() => void createNewTerminalSession()}>
                      新建终端
                    </button>
                  </FloatingError>
                ) : terminalAvailable && terminalSession?.state === 'creating' ? (
                  <p className="panel-loading" role="status">终端正在创建，确认就绪后会自动连接…</p>
                ) : controlError ? (
                  controlNetworkUnavailable
                    ? <p className="panel-loading" role="status">正在重新确认电脑控制状态…</p>
                    : <FloatingError>控制状态暂不可用：{controlError}</FloatingError>
                ) : terminalAvailable ? (
                  <p className="panel-loading" role="status">正在连接终端…</p>
                ) : (
                  <p className="panel-loading" role="status">请先接管电脑后使用终端。</p>
                )}
              </div>
            )}
            {loadedTabs.has('files') && (
              <div className="tab-panel tab-panel-files" aria-hidden={visibleActive !== 'files'} inert={visibleActive !== 'files'}>
                {embeddedFileSwitch()}
                {embedded && !hasEmbedControlTarget(embedTargets)
                  ? <p className="panel-loading" role="status">此会话未授权访问云端电脑文件。</p>
                  : vscodeAvailable
                  ? <FileManager />
                  : controlError
                    ? controlNetworkUnavailable
                      ? <p className="panel-loading" role="status">正在重新确认电脑控制状态…</p>
                      : <FloatingError>控制状态暂不可用：{controlError}</FloatingError>
                    : <p className="panel-loading" role="status">请先接管电脑后使用文件。</p>}
              </div>
            )}
            {loadedTabs.has('workspace') && (
              <div className="tab-panel tab-panel-workspace" aria-hidden={visibleActive !== 'workspace'} inert={visibleActive !== 'workspace'}>
                {embeddedFileSwitch()}
                {embedded && !hasEmbedControlTarget(embedTargets)
                  ? <p className="panel-loading" role="status">此会话未授权访问云端电脑文件。</p>
                  : embedded && !vscodeAvailable
                  ? controlError
                    ? controlNetworkUnavailable
                      ? <p className="panel-loading" role="status">正在重新确认电脑控制状态…</p>
                      : <FloatingError>控制状态暂不可用：{controlError}</FloatingError>
                    : <p className="panel-loading" role="status">请先接管电脑后使用 Workspace。</p>
                  : <WorkspaceBrowser active={visibleActive === 'workspace'} />}
              </div>
            )}
            {loadedTabs.has('vscode') && (
              <div className="tab-panel tab-panel-vscode" aria-hidden={visibleActive !== 'vscode'} inert={visibleActive !== 'vscode'}>
                {vscodeAvailable
                  ? <>
                    <iframe
                      key={vscodeFrameRetry}
                      ref={vscodeFrameRef}
                      title="VSCode"
                      src={vscodeURL}
                      onLoad={(event) => {
                        const frame = event.currentTarget;
                        void confirmFrameResponse(vscodeURL, frame, '.monaco-workbench', 'VS Code')
                          .then(() => {
                            if (vscodeFrameRef.current !== frame) return;
                            setVscodeFrameError('');
                            setVscodeFrameStatus('loaded');
                          })
                          .catch((error: unknown) => {
                            if (vscodeFrameRef.current !== frame) return;
                            setVscodeFrameError(messageOf(error));
                            setVscodeFrameStatus('failed');
                          });
                      }}
                      onError={(event) => {
                        if (vscodeFrameRef.current !== event.currentTarget) return;
                        setVscodeFrameError('VS Code 页面加载失败');
                        setVscodeFrameStatus('failed');
                      }}
                    />
                    {vscodeFrameStatus !== 'loaded' && (
                      <div className="connection-overlay" role={vscodeFrameStatus === 'failed' ? 'alert' : 'status'}>
                        {vscodeFrameStatus === 'loading' ? <>
                          <span className="connection-spinner" aria-hidden="true" />
                          <p>正在连接 VS Code…</p>
                        </> : <>
                          <p>{vscodeFrameError || 'VS Code 连接失败，请稍后重试。'}</p>
                          <button className="button button-secondary" type="button" onClick={() => {
                            setVscodeFrameError('');
                            setVscodeFrameStatus('loading');
                            setVscodeFrameRetry((value) => value + 1);
                          }}>重试 VS Code</button>
                        </>}
                      </div>
                    )}
                  </>
                  : controlError
                    ? controlNetworkUnavailable
                      ? <p className="panel-loading" role="status">正在重新确认电脑控制状态…</p>
                      : <FloatingError>控制状态暂不可用：{controlError}</FloatingError>
                    : <p className="panel-loading" role="status">请先接管电脑后使用 VSCode。</p>}
              </div>
            )}
            {!workspaceOnly && !embedded && visibleActive === 'browser' && <span className="mobile-status-badge" role="status">{mobileControlStatus}</span>}
          </div>
          {!workspaceOnly && !embedded && (
            <div className="mobile-control-footer" aria-label="电脑控制权">
              {controlError && !controlNetworkUnavailable && <button className="button button-secondary mobile-control-retry" type="button" onClick={reloadControl} disabled={controlLoading}>重试</button>}
              {controlActionLabel && <button
                className="button mobile-control-action"
                type="button"
                onClick={controlComputer}
                disabled={controlActionPending || !!controlError || (transitionPending && !failedTransition)}
              >
                {controlActionLabel === '接管电脑' ? '接管控制权' : controlActionLabel}
              </button>}
            </div>
          )}
        </>
      )}
    </main>
  );
}
