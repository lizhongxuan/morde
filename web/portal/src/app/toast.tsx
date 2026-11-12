import { createContext, useCallback, useContext, useLayoutEffect, useMemo, useState } from 'react';
import { createPortal } from 'react-dom';
import type { ReactNode } from 'react';

type ToastAction = { label: string; onClick: () => void };
type Toast = { id: number; message: string; kind: 'success' | 'error' | 'info'; action?: ToastAction };
type ToastContextValue = {
  notify: (message: string, kind?: Toast['kind'], action?: ToastAction) => number;
  dismiss: (id: number) => void;
  overlayRegion: HTMLDivElement | null;
};
const ToastContext = createContext<ToastContextValue | null>(null);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<Toast[]>([]);
  const [overlayRegion] = useState<HTMLDivElement | null>(() => {
    if (typeof document === 'undefined') return null;
    const region = document.createElement('div');
    region.className = 'toast-region';
    region.setAttribute('aria-live', 'polite');
    region.setAttribute('aria-relevant', 'additions');
    return region;
  });
  useLayoutEffect(() => {
    if (!overlayRegion) return;
    document.body.append(overlayRegion);
    return () => overlayRegion.remove();
  }, [overlayRegion]);
  const dismiss = useCallback((id: number) => setItems((current) => current.filter((item) => item.id !== id)), []);
  const notify = useCallback((message: string, kind: Toast['kind'] = 'info', action?: ToastAction) => {
    const id = Date.now() + Math.random();
    setItems((current) => [...current, { id, message, kind, action }]);
    if (!action) window.setTimeout(() => dismiss(id), 6000);
    return id;
  }, [dismiss]);
  const value = useMemo(() => ({ notify, dismiss, overlayRegion }), [notify, dismiss, overlayRegion]);
  return <ToastContext.Provider value={value}>
    {children}
    {overlayRegion && createPortal(items.map((item) => <div className={`toast toast-${item.kind}`} key={item.id} role={item.kind === 'error' ? 'alert' : 'status'}>{item.message}{item.action && <button type="button" className="button button-small button-secondary" onClick={item.action.onClick}>{item.action.label}</button>}<button type="button" className="icon-button" aria-label="关闭通知" onClick={() => dismiss(item.id)}>×</button></div>), overlayRegion)}
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
