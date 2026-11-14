import { useLayoutEffect, useRef, useState, type HTMLAttributes, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

export default function FloatingError({ children, className = '', role = 'alert', ...props }: HTMLAttributes<HTMLDivElement> & { children: ReactNode }) {
  const [dismissed, setDismissed] = useState(false);
  const [region] = useState<HTMLElement | null>(() => {
    if (typeof document === 'undefined') return null;
    let target = document.getElementById('workbench-notice-region');
    if (!target) {
      target = document.createElement('div');
      target.id = 'workbench-notice-region';
      target.className = 'workbench-notice-region';
      document.body.append(target);
    }
    return target;
  });
  const [sourceVisible, setSourceVisible] = useState(false);
  const anchor = useRef<HTMLSpanElement>(null);
  useLayoutEffect(() => {
    if (!region) return;
    const ancestors: HTMLElement[] = [];
    for (let ancestor = anchor.current?.parentElement ?? null; ancestor; ancestor = ancestor.parentElement) ancestors.push(ancestor);
    const updateVisibility = () => {
      const visible = !ancestors.some((ancestor) => ancestor.hidden || ancestor.getAttribute('aria-hidden') === 'true'
        || getComputedStyle(ancestor).display === 'none' || getComputedStyle(ancestor).visibility === 'hidden');
      setSourceVisible((current) => current === visible ? current : visible);
    };
    updateVisibility();
    const observer = new MutationObserver(updateVisibility);
    ancestors.forEach((ancestor) => observer.observe(ancestor, { attributes: true, attributeFilter: ['aria-hidden', 'class', 'hidden', 'style'] }));
    return () => observer.disconnect();
  }, [region]);
  if (dismissed) return null;
  const content = <div {...props} className={`workbench-floating-error ${className}`.trim()} role={role}>
    <span>{children}</span>
    <button type="button" className="icon-button" aria-label={role === 'alert' ? '关闭错误提示' : '关闭状态提示'} onClick={() => setDismissed(true)}>×</button>
  </div>;
  return <><span ref={anchor} hidden aria-hidden="true" />{region && sourceVisible ? createPortal(content, region) : null}</>;
}
