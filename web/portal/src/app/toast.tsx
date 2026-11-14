import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import type { ReactNode } from 'react';

type ToastAction = { label: string; onClick: () => void };
type Toast = { id: number; message: string; kind: 'success' | 'error' | 'info'; action?: ToastAction };
type ToastContextValue = {
  notify: (message: string, kind?: Toast['kind'], action?: ToastAction) => number;
  dismiss: (id: number) => void;
  overlayRegion: HTMLDivElement | null;
  noticeRegion: HTMLDivElement | null;
  closeNotices: () => void;
};
const ToastContext = createContext<ToastContextValue | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);
  const [noticeCount, setNoticeCount] = useState(0);
  const [noticeLabel, setNoticeLabel] = useState('状态提示');
  const noticeDetails = useRef<HTMLDetailsElement>(null);
  const [overlayRegion] = useState<HTMLDivElement | null>(() => {
    if (typeof document === 'undefined') return null;
    const region = document.createElement('div');
    region.className = 'toast-region';
    region.setAttribute('aria-live', 'polite');
    region.setAttribute('aria-relevant', 'additions');
    return region;
  });
  const [noticeRegion] = useState<HTMLDivElement | null>(() => typeof document === 'undefined' ? null : document.createElement('div'));
  useLayoutEffect(() => {
    if (!overlayRegion) return;
    document.body.append(overlayRegion);
    return () => overlayRegion.remove();
  }, [overlayRegion]);
  useLayoutEffect(() => {
    if (!noticeRegion) return;
    const update = () => {
      setNoticeCount(noticeRegion.childElementCount);
      setNoticeLabel(noticeRegion.querySelector('[data-notice-label]')?.getAttribute('data-notice-label') || '状态提示');
    };
    const observer = new MutationObserver(update);
    observer.observe(noticeRegion, { childList: true });
    update();
    return () => observer.disconnect();
  }, [noticeRegion]);
  const dismiss = useCallback((id: number) => setItems((current) => current.filter((item) => item.id !== id)), []);
  const closeNotices = useCallback(() => { if (noticeDetails.current) noticeDetails.current.open = false; }, []);
  const notify = useCallback((message: string, kind: Toast['kind'] = 'info', action?: ToastAction) => {
    const id = Date.now() + Math.random();
    setItems((current) => [...current, { id, message, kind, action }]);
    if (!action) window.setTimeout(() => dismiss(id), 6000);
    return id;
  }, [dismiss]);
  const value = useMemo(() => ({ notify, dismiss, overlayRegion, noticeRegion, closeNotices }), [notify, dismiss, overlayRegion, noticeRegion, closeNotices]);
  return <ToastContext.Provider value={value}>
    {children}
    {overlayRegion && createPortal(items.map((item) => <div className={`toast toast-${item.kind}`} key={item.id} role={item.kind === 'error' ? 'alert' : 'status'}>{item.message}{item.action && <button type="button" className="button button-small button-secondary" onClick={item.action.onClick}>{item.action.label}</button>}<button type="button" className="icon-button" aria-label="关闭通知" onClick={() => dismiss(item.id)}>×</button></div>), overlayRegion)}
    {noticeRegion && typeof document !== 'undefined' && createPortal(<div className="notice-center" hidden={!noticeCount}>
      <details ref={noticeDetails}><summary aria-label={`${noticeLabel}，${noticeCount} 条`}><span>{noticeLabel}</span><span aria-live="polite">{noticeCount}</span></summary>
        <div className="notice-center-panel" ref={(element) => { if (element && noticeRegion.parentElement !== element) element.append(noticeRegion); }} />
      </details>
    </div>, document.body)}
  </ToastContext.Provider>;
}

export function useToast() {
  const value = useContext(ToastContext);
  if (!value) throw new Error('useToast 必须在 ToastProvider 内使用');
  return value;
}

export function useToastOverlayRegion() {
  return useContext(ToastContext)?.overlayRegion ?? null;
}

export function useNoticeOverlayRegion() {
  return useContext(ToastContext)?.noticeRegion ?? null;
}
