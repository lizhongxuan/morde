import { createContext, useCallback, useContext, useMemo, useRef, useState, type RefObject } from 'react';
import type { ReactNode } from 'react';

type ToastAction = { label: string; onClick: () => void };
type Toast = { id: number; message: string; kind: 'success' | 'error' | 'info'; action?: ToastAction };
type ToastContextValue = {
  items: Toast[];
  notify: (message: string, kind?: Toast['kind'], action?: ToastAction) => number;
  dismiss: (id: number) => void;
  noticeDetails: RefObject<HTMLDetailsElement | null>;
  closeNotices: () => void;
};
const ToastContext = createContext<ToastContextValue | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);
  const noticeDetails = useRef<HTMLDetailsElement>(null);
  const dismiss = useCallback((id: number) => setItems((current) => current.filter((item) => item.id !== id)), []);
  const closeNotices = useCallback(() => { if (noticeDetails.current) noticeDetails.current.open = false; }, []);
  const notify = useCallback((message: string, kind: Toast['kind'] = 'info', action?: ToastAction) => {
    const id = Date.now() + Math.random();
    setItems((current) => [...current, { id, message, kind, action }]);
    if (!action) window.setTimeout(() => dismiss(id), 6000);
    return id;
  }, [dismiss]);
  const value = useMemo(() => ({ items, notify, dismiss, noticeDetails, closeNotices }), [items, notify, dismiss, closeNotices]);
  return <ToastContext.Provider value={value}>{children}</ToastContext.Provider>;
}

export function ToastNoticeCenter() {
  const { items, dismiss, noticeDetails } = useToast();
  const errors = items.filter((item) => item.kind === 'error').length;
  const notices = items.length - errors;
  const label = errors && notices ? '错误与状态' : errors ? '错误提示' : '状态提示';
  return <div className="notice-center" hidden={!items.length}>
    <details ref={noticeDetails}><summary aria-label={`${label}，${items.length} 条`}><span aria-live="polite">{label}</span><span aria-live="polite">{items.length}</span></summary>
      <div className="notice-center-panel">{items.map((item) => <div className={`toast-notice toast-${item.kind}`} key={item.id} role={item.kind === 'error' ? 'alert' : 'status'}>
        {item.message}{item.action && <button type="button" className="button button-small button-secondary" onClick={item.action.onClick}>{item.action.label}</button>}
        <button type="button" className="icon-button" aria-label="关闭通知" onClick={() => dismiss(item.id)}>×</button>
      </div>)}</div>
    </details>
  </div>;
}

export function useToast() {
  const value = useContext(ToastContext);
  if (!value) throw new Error('useToast 必须在 ToastProvider 内使用');
  return value;
}
