import { useCallback, useEffect, useRef, useState } from 'react';
import {
  gatewayApi,
  type GatewayControl,
  type GatewayControlAction,
  type GatewayControlRequest,
  type GatewayHost,
  type GatewayTerminalSession,
} from '../api/gateway';
import FileManager from '../components/FileManager';
import TopBar from '../components/TopBar';

type Tab = 'browser' | 'terminal' | 'files' | 'vscode';
const primaryTabs: { id: Tab; label: string }[] = [
  { id: 'browser', label: '桌面' },
  { id: 'terminal', label: '终端' },
  { id: 'files', label: '文件' },
  { id: 'vscode', label: 'VS Code' },
];

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : '请求失败，请稍后重试';
}

function requestId() {
  return globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function controlActionCompleted(control: GatewayControl, action: GatewayControlAction) {
  const target = action === 'take_over' ? 'user' : 'ai';
  return control.control_state === target && !control.pending_operation_ids?.length &&
    (!control.active_transition || control.active_transition.phase === 'completed');
}

export default function Workbench() {
  const [host, setHost] = useState<GatewayHost | null>(null);
  const [control, setControl] = useState<GatewayControl | null>(null);
  const [controlLoading, setControlLoading] = useState(true);
  const [controlError, setControlError] = useState('');
  const [controlActionError, setControlActionError] = useState('');
  const [controlActionPending, setControlActionPending] = useState(false);
  const [controlRequest, setControlRequest] = useState<GatewayControlRequest | null>(null);
  const [pageError, setPageError] = useState('');
  const [active, setActive] = useState<Tab>('browser');
  const [loaded, setLoaded] = useState<Set<Tab>>(() => new Set(['browser']));
  const [browserStatus, setBrowserStatus] = useState<'connecting' | 'connected' | 'disconnected' | 'failed'>('connecting');
  const [browserReconnect, setBrowserReconnect] = useState(0);
  const [browserFullscreen, setBrowserFullscreen] = useState(false);
  const [showWindowHelp, setShowWindowHelp] = useState(false);
  const [terminalSession, setTerminalSession] = useState<GatewayTerminalSession | null>(null);
  const [terminalError, setTerminalError] = useState('');
  const [terminalRequestPending, setTerminalRequestPending] = useState(false);
  const [renewing, setRenewing] = useState(false);
  const [notice, setNotice] = useState('');
  const [noticeKind, setNoticeKind] = useState<'success' | 'error'>('success');
  const browserFrameRef = useRef<HTMLIFrameElement>(null);
  const browserPanelRef = useRef<HTMLDivElement>(null);
  const browserObserverRef = useRef<MutationObserver | null>(null);
  const browserConnectTimeoutRef = useRef<number | undefined>(undefined);
  const browserConnectedRef = useRef(false);
  const controlRequestRef = useRef<GatewayControlRequest | null>(null);
  const terminalRequestRef = useRef(false);

  const applyControl = useCallback((value: GatewayControl) => {
    setControl(value);
    setControlError('');
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

  useEffect(() => {
    let current = true;
    gatewayApi.host()
      .then((value) => current && setHost(value))
      .catch((error: unknown) => current && setPageError(messageOf(error)));
    gatewayApi.control()
      .then((value) => current && applyControl(value))
      .catch((error: unknown) => current && setControlError(messageOf(error)))
      .finally(() => current && setControlLoading(false));
    return () => {
      current = false;
    };
  }, [applyControl]);

  const hasControl = control !== null;

  useEffect(() => {
    const transition = control?.control_state === 'taking_over' || control?.control_state === 'returning';
    const interval = (controlActionPending || (transition && !control?.active_transition?.failure_reason)) ? 1000 : 3000;
    let current = true;
    const refresh = async () => {
      try {
        const value = await gatewayApi.control();
        if (current) applyControl(value);
      } catch (error) {
        if (current && !hasControl) setControlError(messageOf(error));
      }
    };
    const timer = window.setInterval(() => void refresh(), interval);
    return () => {
      current = false;
      window.clearInterval(timer);
    };
  }, [applyControl, control?.active_transition?.failure_reason, control?.control_state, controlActionPending, hasControl]);

  const stopBrowserWatch = useCallback(() => {
    browserObserverRef.current?.disconnect();
    browserObserverRef.current = null;
    if (browserConnectTimeoutRef.current !== undefined) {
      window.clearTimeout(browserConnectTimeoutRef.current);
      browserConnectTimeoutRef.current = undefined;
    }
  }, []);

  useEffect(() => () => stopBrowserWatch(), [stopBrowserWatch]);

  useEffect(() => {
    const onFullscreenChange = () => setBrowserFullscreen(document.fullscreenElement === browserPanelRef.current);
    document.addEventListener('fullscreenchange', onFullscreenChange);
    return () => document.removeEventListener('fullscreenchange', onFullscreenChange);
  }, []);

  const terminalAvailable = !controlLoading && !controlError && !controlActionPending && control?.control_state === 'user';

  useEffect(() => {
    if (active !== 'terminal' || !terminalAvailable || terminalSession || terminalError || terminalRequestRef.current) return;
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
      })
      .catch((error: unknown) => {
        if (current) setTerminalError(messageOf(error));
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
  }, [active, terminalAvailable, terminalSession, terminalError]);

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
      } catch (error) {
        if (current) setTerminalError(messageOf(error));
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
  }, [active, terminalAvailable, terminalError, terminalSession?.state]);

  async function createNewTerminalSession() {
    if (terminalRequestRef.current || !window.confirm('新终端不会停止原终端中的命令。请先保存工作；原会话可能继续运行并占用资源。确定新建吗？')) return;
    terminalRequestRef.current = true;
    setTerminalSession(null);
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
    } catch (error) {
      setTerminalError(messageOf(error));
    } finally {
      terminalRequestRef.current = false;
      setTerminalRequestPending(false);
    }
  }

  function selectTab(tab: Tab) {
    if (tab === 'vscode' && !vscodeAvailable) return;
    setActive(tab);
    setLoaded((previous) => new Set(previous).add(tab));
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
    if (control?.control_state !== 'user' || browserStatus !== 'connected') return;
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
      setNoticeKind('error');
      setNotice(messageOf(error));
    } finally {
      setRenewing(false);
    }
  }

  async function controlComputer() {
    if (!control) return;
    const transition = control.active_transition;
    const retryTransition = transition?.failure_reason ? transition : null;
    const current = controlRequestRef.current;
    const action = current?.action ?? retryTransition?.action ?? (control.control_state === 'user' ? 'release' : 'take_over');
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
    try {
      applyControl(await gatewayApi.controlAction(request));
    } catch (error) {
      setControlActionError(messageOf(error));
      try {
        applyControl(await gatewayApi.control());
      } catch (statusError) {
        setControlError(messageOf(statusError));
      }
    } finally {
      setControlActionPending(false);
    }
  }

  async function reloadControl() {
    setControlLoading(true);
    try {
      applyControl(await gatewayApi.control());
    } catch (error) {
      setControlError(messageOf(error));
    } finally {
      setControlLoading(false);
    }
  }

  const failedTransition = control?.active_transition?.failure_reason ? control.active_transition : null;
  const vscodeAvailable = !controlLoading && !controlError && control?.control_state === 'user' && !controlActionPending;
  const browserViewOnly = controlLoading || !!controlError || (control?.control_state !== 'user' && control?.control_state !== 'returning');
  const retryControlRequest = controlRequest ?? (failedTransition ? {
    action: failedTransition.action,
    request_id: failedTransition.request_id,
    expected_control_version: failedTransition.expected_control_version,
  } : null);
  const controlActionLabel = controlActionPending && controlRequest
    ? controlRequest.action === 'take_over' ? '接管中…' : '交回中…'
    : (controlActionError || failedTransition) && retryControlRequest
      ? retryControlRequest.action === 'take_over' ? '重试接管' : '重试交回 AI'
      : control?.control_state === 'ai' ? '接管电脑'
        : control?.control_state === 'user' ? '交回 AI' : '';
  const visibleActive = active === 'vscode' && !vscodeAvailable ? 'browser' : active;
  return (
    <main className="workbench-app">
      <TopBar
        host={host}
        renewing={renewing}
        onRenew={renew}
        control={control}
        controlLoading={controlLoading}
        controlError={controlError}
        controlActionError={controlActionError}
        controlActionLabel={controlActionLabel}
        controlActionPending={controlActionPending}
        onControlAction={controlComputer}
        onRetryControl={reloadControl}
      />
      {notice && <p className={`notice${noticeKind === 'error' ? ' notice-error' : ''}`} role={noticeKind === 'error' ? 'alert' : 'status'}>{notice}</p>}
      {pageError ? (
        <section className="page-message" role="alert">
          <h1>工作台暂时无法加载</h1>
          <p>{pageError}</p>
          <button className="button" onClick={() => window.location.reload()}>重新加载</button>
        </section>
      ) : (
        <>
          <nav className="workbench-tabs" aria-label="工作台功能">
            <div className="primary-tabs">
              {primaryTabs.map((tab) => (
                <button
                  key={tab.id}
                  className={`tab-button${active === tab.id ? ' is-active' : ''}`}
                  type="button"
                  aria-current={visibleActive === tab.id ? 'page' : undefined}
                  disabled={tab.id === 'vscode' && !vscodeAvailable}
                  title={tab.id === 'vscode' && !vscodeAvailable ? '请先接管电脑' : undefined}
                  onClick={() => selectTab(tab.id)}
                >
                  {tab.label}
                </button>
              ))}
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
          </nav>
          <div className="workbench-content">
            {loaded.has('browser') && (
              <div ref={browserPanelRef} className="tab-panel tab-panel-browser" aria-hidden={visibleActive !== 'browser'} inert={visibleActive !== 'browser'}>
                {browserStatus !== 'connected' && (
                  <div className="connection-overlay" role={browserStatus === 'failed' || browserStatus === 'disconnected' ? 'alert' : 'status'}>
                    <p>{browserStatus === 'failed' ? '远程桌面没有返回连接状态，请确认主机正在运行后重试。'
                      : browserStatus === 'disconnected' ? '远程桌面连接已中断，可以重新连接当前主机。'
                        : '正在连接远程桌面…'}</p>
                    {(browserStatus === 'failed' || browserStatus === 'disconnected') && (
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
                  src={`/vnc/index.html?autoconnect=true&reconnect=true&resize=scale&view_only=${browserViewOnly}`}
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
            {loaded.has('terminal') && (
              <div className="tab-panel tab-panel-terminal" aria-hidden={visibleActive !== 'terminal'} inert={visibleActive !== 'terminal'}>
                {terminalError ? (
                  <div className="panel-error" role="alert">
                    <p>{terminalError}</p>
                    <button className="button button-secondary" type="button" onClick={() => setTerminalError('')}>
                      重新连接
                    </button>
                  </div>
                ) : terminalAvailable && terminalSession?.state === 'ready' ? (
                  <>
                    {visibleActive === 'terminal' && (
                      <div className="terminal-actions">
                        <button className="button button-secondary" type="button" disabled={terminalRequestPending} onClick={() => void createNewTerminalSession()}>
                          新建终端
                        </button>
                      </div>
                    )}
                    {visibleActive === 'terminal' && <iframe title="主机终端" src={terminalSession.url} />}
                  </>
                ) : terminalAvailable && (terminalSession?.state === 'unknown' || terminalSession?.state === 'interrupted') ? (
                  <div className="panel-error" role="alert">
                    <p>{terminalSession.state === 'unknown'
                      ? '原终端的创建结果不确定，可能仍有命令在运行。系统不会自动创建第二个终端。'
                      : '原终端会话已中断或无法恢复。若原命令可能仍在运行，请先核实状态；系统不会自动创建第二个终端。'}</p>
                    <button className="button button-secondary" type="button" disabled={terminalRequestPending} onClick={() => void createNewTerminalSession()}>
                      新建终端
                    </button>
                  </div>
                ) : terminalAvailable && terminalSession?.state === 'creating' ? (
                  <p className="panel-loading" role="status">终端正在创建，确认就绪后会自动连接…</p>
                ) : controlError ? (
                  <p className="panel-error" role="alert">{controlError}</p>
                ) : terminalAvailable ? (
                  <p className="panel-loading" role="status">正在连接终端…</p>
                ) : (
                  <p className="panel-loading" role="status">请先接管电脑后使用终端。</p>
                )}
              </div>
            )}
            {loaded.has('files') && (
              <div className="tab-panel tab-panel-files" aria-hidden={visibleActive !== 'files'} inert={visibleActive !== 'files'}>
                <FileManager />
              </div>
            )}
            {loaded.has('vscode') && (
              <div className="tab-panel tab-panel-vscode" aria-hidden={visibleActive !== 'vscode'} inert={visibleActive !== 'vscode'}>
                {vscodeAvailable
                  ? <iframe title="VSCode" src="/code-server/" />
                  : <p className="panel-loading" role="status">请先接管电脑后使用 VSCode。</p>}
              </div>
            )}
          </div>
        </>
      )}
    </main>
  );
}
