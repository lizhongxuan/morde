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
    setItems((current) => [...current, { id, message, kind, action }]);
    if (!action) window.setTimeout(() => dismiss(id), 6000);
    return id;
  }, [dismiss]);
  const modalTarget = modalTargets.at(-1) ?? null;
  const value = useMemo(() => ({ items, notify, dismiss, noticeDetails, noticesOpen, setNoticesOpen, modalTarget, registerModal, openNotices, closeNotices }), [items, notify, dismiss, noticesOpen, modalTarget, registerModal, openNotices, closeNotices]);
  return <ToastContext.Provider value={value}>{children}</ToastContext.Provider>;
}

export function ToastNoticeCenter({ autoOpenErrors = false, overlay = false }: { autoOpenErrors?: boolean; overlay?: boolean } = {}) {
  const { items, dismiss, noticeDetails, noticesOpen, setNoticesOpen, modalTarget, openNotices } = useToast();
  const openedErrorIDs = useRef(new Set<number>());
  useEffect(() => {
    const newError = items.some((item) => item.kind === 'error' && !openedErrorIDs.current.has(item.id));
    for (const item of items) if (item.kind === 'error') openedErrorIDs.current.add(item.id);
    if (autoOpenErrors && newError) openNotices();
  }, [autoOpenErrors, items, openNotices, noticeDetails]);
  const errors = items.filter((item) => item.kind === 'error').length;
  const notices = items.length - errors;
  const label = errors && notices ? '错误与状态' : errors ? '错误提示' : '状态提示';
  const center = <div className={`notice-center${overlay || modalTarget ? ' notice-center-overlay' : ''}`} hidden={!items.length}>
    <details ref={noticeDetails} open={noticesOpen} onToggle={(event) => setNoticesOpen(event.currentTarget.open)}><summary aria-label={`${label}，${items.length} 条`}><span aria-live="polite">{label}</span><span aria-live="polite">{items.length}</span></summary>
      <div className="notice-center-panel">{items.map((item) => <div className={`toast-notice toast-${item.kind}`} key={item.id} role={item.kind === 'error' ? 'alert' : 'status'}>
        {item.message}{item.action && <button type="button" className="button button-small button-secondary" onClick={item.action.onClick}>{item.action.label}</button>}
        <button type="button" className="icon-button" aria-label="关闭通知" onClick={() => dismiss(item.id)}>×</button>
      </div>)}</div>
    </details>
  </div>;
  return modalTarget ? createPortal(center, modalTarget) : center;
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
