import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useRef, useState, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import type { ReactNode } from 'react';

type ToastAction = { label: string; onClick: () => void };
type Toast = { id: number; message: string; kind: 'success' | 'error' | 'info'; action?: ToastAction };
type ToastContextValue = {
  notify: (message: string, kind?: Toast['kind'], action?: ToastAction) => number;
  dismiss: (id: number) => void;
  noticeRegion: HTMLDivElement | null;
  noticeCount: number;
  noticeLabel: string;
  noticeDetails: RefObject<HTMLDetailsElement | null>;
  closeNotices: () => void;
};
const ToastContext = createContext<ToastContextValue | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);
  const [noticeCount, setNoticeCount] = useState(0);
  const [noticeLabel, setNoticeLabel] = useState('状态提示');
  const noticeDetails = useRef<HTMLDetailsElement>(null);
  const [noticeRegion] = useState<HTMLDivElement | null>(() => typeof document === 'undefined' ? null : document.createElement('div'));
  useLayoutEffect(() => {
    if (!noticeRegion) return;
    const update = () => {
      setNoticeCount(noticeRegion.childElementCount);
      const errors = noticeRegion.querySelectorAll('.toast-error-notice, .toast-error').length;
      const notices = noticeRegion.childElementCount - errors;
      setNoticeLabel(errors && notices ? '错误与状态' : errors ? '错误提示' : noticeRegion.querySelector('[data-notice-label]')?.getAttribute('data-notice-label') || '状态提示');
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
  const value = useMemo(() => ({ notify, dismiss, noticeRegion, noticeCount, noticeLabel, noticeDetails, closeNotices }), [notify, dismiss, noticeRegion, noticeCount, noticeLabel, closeNotices]);
  return <ToastContext.Provider value={value}>
    {children}
    {noticeRegion && createPortal(items.map((item) => <div className={`toast-notice toast-${item.kind}${item.kind === 'error' ? ' toast-error-notice' : ''}`} key={item.id} role={item.kind === 'error' ? 'alert' : 'status'}>{item.message}{item.action && <button type="button" className="button button-small button-secondary" onClick={item.action.onClick}>{item.action.label}</button>}<button type="button" className="icon-button" aria-label="关闭通知" onClick={() => dismiss(item.id)}>×</button></div>), noticeRegion)}
  </ToastContext.Provider>;
}

export function ToastNoticeCenter() {
  const { noticeRegion, noticeCount, noticeLabel, noticeDetails } = useToast();
  return <div className="notice-center" hidden={!noticeCount}>
    <details ref={noticeDetails}><summary aria-label={`${noticeLabel}，${noticeCount} 条`}><span aria-live="polite">{noticeLabel}</span><span aria-live="polite">{noticeCount}</span></summary>
      <div className="notice-center-panel" ref={(element) => { if (element && noticeRegion && noticeRegion.parentElement !== element) element.append(noticeRegion); }} />
    </details>
  </div>;
}

export function useToast() {
  const value = useContext(ToastContext);
  if (!value) throw new Error('useToast 必须在 ToastProvider 内使用');
  return value;
}

export function useNoticeOverlayRegion() {
  return useContext(ToastContext)?.noticeRegion ?? null;
}
