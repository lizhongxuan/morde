import { useLayoutEffect, useRef, useState, type HTMLAttributes, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

export default function FloatingError({ children, ...props }: HTMLAttributes<HTMLDivElement> & { children: ReactNode }) {
  const [dismissed, setDismissed] = useState(false);
  const [sourceVisible, setSourceVisible] = useState(false);
  const anchor = useRef<HTMLSpanElement>(null);
  const target = typeof document === 'undefined' ? null : document.getElementById('workbench-error-region') ?? document.body;

  useLayoutEffect(() => {
    const ancestors: HTMLElement[] = [];
    for (let ancestor = anchor.current?.parentElement ?? null; ancestor; ancestor = ancestor.parentElement) ancestors.push(ancestor);
    const updateVisibility = () => {
      const visible = !ancestors.some((ancestor) => ancestor.hidden || ancestor.inert || ancestor.getAttribute('aria-hidden') === 'true'
        || getComputedStyle(ancestor).display === 'none' || getComputedStyle(ancestor).visibility === 'hidden');
      setSourceVisible((current) => current === visible ? current : visible);
    };
    updateVisibility();
    const observer = new MutationObserver(updateVisibility);
    ancestors.forEach((ancestor) => observer.observe(ancestor, { attributes: true, attributeFilter: ['aria-hidden', 'class', 'hidden', 'inert', 'style'] }));
    return () => observer.disconnect();
  }, [target]);

  return <>
    <span ref={anchor} hidden aria-hidden="true" />
    {target && sourceVisible && !dismissed && createPortal(<div {...props} className="workbench-floating-error" role="alert">
      <span>{children}</span>
      <button type="button" className="icon-button" aria-label="关闭错误提示" onClick={() => setDismissed(true)}>×</button>
    </div>, target)}
  </>;
}
