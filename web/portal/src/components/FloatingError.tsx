import { useLayoutEffect, useRef, useState, type HTMLAttributes, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { useToastOverlayRegion } from '../app/toast';

export function FloatingError({ children, className = '', role = 'alert', ...props }: HTMLAttributes<HTMLDivElement> & { children: ReactNode }) {
  const overlayRegion = useToastOverlayRegion();
  const [dismissed, setDismissed] = useState(false);
  const [sourceVisible, setSourceVisible] = useState(false);
  const anchor = useRef<HTMLSpanElement>(null);
  const target = overlayRegion ?? (typeof document === 'undefined' ? null : document.body);
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
    {target && sourceVisible && !dismissed && createPortal(<div {...props} className={`toast-error-notice ${className}`.trim()} role={role}>
    {children}
    <button type="button" className="icon-button" aria-label="关闭错误提示" onClick={() => setDismissed(true)}>×</button>
    </div>, target)}
  </>;
}
