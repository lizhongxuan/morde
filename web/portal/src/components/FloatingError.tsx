import { useLayoutEffect, useRef, useState, type HTMLAttributes, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { useNoticeOverlayRegion, useToastOverlayRegion } from '../app/toast';

type OverlayProps = HTMLAttributes<HTMLDivElement> & { children: ReactNode };

function FloatingOverlay({ children, className = '', role, region, dismissible, ...props }: OverlayProps & { region: HTMLElement | null; dismissible: boolean }) {
  const [dismissed, setDismissed] = useState(false);
  const [sourceVisible, setSourceVisible] = useState(false);
  const anchor = useRef<HTMLSpanElement>(null);
  const target = region ?? (typeof document === 'undefined' ? null : document.body);
  useLayoutEffect(() => {
    const ancestors: HTMLElement[] = [];
    for (let ancestor = anchor.current?.parentElement ?? null; ancestor; ancestor = ancestor.parentElement) ancestors.push(ancestor);
    const updateVisibility = () => {
      const visible = !ancestors.some((ancestor) => ancestor.hidden || getComputedStyle(ancestor).display === 'none');
      setSourceVisible((current) => current === visible ? current : visible);
    };
    updateVisibility();
    const observer = new MutationObserver(updateVisibility);
    ancestors.forEach((ancestor) => observer.observe(ancestor, { attributes: true, attributeFilter: ['class', 'hidden', 'style'] }));
    return () => observer.disconnect();
  }, [target]);
  return <>
    <span ref={anchor} hidden aria-hidden="true" />
    {target && sourceVisible && !dismissed && createPortal(<div {...props} className={className} role={role}>
    {children}
    {dismissible && <button type="button" className="icon-button" aria-label="关闭错误提示" onClick={() => setDismissed(true)}>×</button>}
    </div>, target)}
  </>;
}

export function FloatingError({ className = '', role = 'alert', ...props }: OverlayProps) {
  const region = useToastOverlayRegion();
  return <FloatingOverlay {...props} className={`toast-error-notice ${className}`.trim()} role={role} region={region} dismissible />;
}

export function FloatingNotice({ className = '', role = 'status', ...props }: OverlayProps) {
  const region = useNoticeOverlayRegion();
  return <FloatingOverlay {...props} className={`notice ${className}`.trim()} role={role} region={region} dismissible={false} />;
}
