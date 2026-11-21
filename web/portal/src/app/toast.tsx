import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type RefObject } from 'react';
import type { ReactNode } from 'react';
import { createPortal } from 'react-dom';

type ToastAction = { label: string; onClick: () => void };
type Toast = { id: number; message: string; kind: 'success' | 'error' | 'info'; action?: ToastAction };
type ToastContextValue = {
  items: Toast[];
  notify: (message: string, kind?: Toast['kind'], action?: ToastAction) => number;
  dismiss: (id: number) => void;
  noticeDetails: RefObject<HTMLDetailsElement | null>;
  noticesOpen: boolean;
  setNoticesOpen: (open: boolean) => void;
  modalTarget: HTMLElement | null;
  registerModal: (target: HTMLElement) => () => void;
  openNotices: () => void;
  closeNotices: () => void;
};
const ToastContext = createContext<ToastContextValue | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);
  const [noticesOpen, setNoticesOpen] = useState(false);
  const [modalTargets, setModalTargets] = useState<HTMLElement[]>([]);
  const noticeDetails = useRef<HTMLDetailsElement>(null);
  const dismiss = useCallback((id: number) => setItems((current) => current.filter((item) => item.id !== id)), []);
  const openNotices = useCallback(() => setNoticesOpen(true), []);
  const closeNotices = useCallback(() => setNoticesOpen(false), []);
  const registerModal = useCallback((target: HTMLElement) => {
    setModalTargets((current) => [...current, target]);
    return () => setModalTargets((current) => current.filter((item) => item !== target));
  }, []);
  const notify = useCallback((message: string, kind: Toast['kind'] = 'info', action?: ToastAction) => {
    const id = Date.now() + Math.random();
    setItems((current) => [...(kind === 'error' ? current.filter((item) => item.kind !== 'error') : current), { id, message, kind, ...(kind === 'error' || !action ? {} : { action }) }]);
    if (kind !== 'error' && !action) window.setTimeout(() => dismiss(id), 6000);
    return id;
  }, [dismiss]);
  const modalTarget = modalTargets.at(-1) ?? null;
  const value = useMemo(() => ({ items, notify, dismiss, noticeDetails, noticesOpen, setNoticesOpen, modalTarget, registerModal, openNotices, closeNotices }), [items, notify, dismiss, noticesOpen, modalTarget, registerModal, openNotices, closeNotices]);
  return <ToastContext.Provider value={value}>{children}</ToastContext.Provider>;
}

export function ToastNoticeCenter({ overlay = false }: { autoOpenErrors?: boolean; overlay?: boolean } = {}) {
  const { items, dismiss, noticeDetails, noticesOpen, setNoticesOpen, modalTarget } = useToast();
  const error = items.find((item) => item.kind === 'error');
  const notices = items.filter((item) => item.kind !== 'error');
  const label = '状态提示';
  const center = <div className={`notice-center${overlay || modalTarget ? ' notice-center-overlay' : ''}`} hidden={!notices.length}>
    <details ref={noticeDetails} open={noticesOpen} onToggle={(event) => setNoticesOpen(event.currentTarget.open)}><summary aria-label={`${label}，${notices.length} 条`}><span aria-live="polite">{label}</span><span aria-live="polite">{notices.length}</span></summary>
      <div className="notice-center-panel">{notices.map((item) => <div className={`toast-notice toast-${item.kind}`} key={item.id} role="status">
        {item.message}{item.action && <button type="button" className="button button-small button-secondary" onClick={item.action.onClick}>{item.action.label}</button>}
        <button type="button" className="icon-button" aria-label="关闭通知" onClick={() => dismiss(item.id)}>×</button>
      </div>)}</div>
    </details>
  </div>;
  const errorOverlay = error && <div className="error-notice-overlay"><div className="toast-notice toast-error" role="alert">
    {error.message}<button type="button" className="icon-button" aria-label="关闭错误提示" onClick={() => dismiss(error.id)}>×</button>
  </div></div>;
  return <>{errorOverlay}{modalTarget ? createPortal(center, modalTarget) : center}</>;
}

export function useModalNoticeTarget(ref: RefObject<HTMLElement | null>) {
  const registerModal = useContext(ToastContext)?.registerModal;
  useEffect(() => {
    if (registerModal && ref.current) return registerModal(ref.current);
  }, [ref, registerModal]);
}

export function useToast() {
  const value = useContext(ToastContext);
  if (!value) throw new Error('useToast 必须在 ToastProvider 内使用');
  return value;
}

export function useOptionalToast() {
  return useContext(ToastContext);
}
