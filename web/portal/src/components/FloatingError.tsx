import { Children, isValidElement, useEffect, useRef, useState, type HTMLAttributes, type ReactNode } from 'react';
import { useOptionalToast } from '../app/toast';

type NoticeProps = HTMLAttributes<HTMLDivElement> & { children: ReactNode };

function errorText(node: ReactNode): string {
  if (typeof node === 'string' || typeof node === 'number') return String(node);
  if (isValidElement<{ children?: ReactNode }>(node)) return node.type === 'button' ? '' : errorText(node.props.children);
  return Children.toArray(node).map(errorText).join('');
}

export function FloatingError({ children, className = '', role = 'alert', ...props }: NoticeProps) {
  const [dismissed, setDismissed] = useState(false);
  const toast = useOptionalToast();
  const message = errorText(children).replace(/\s+/g, ' ').trim();
  const toastID = useRef<number | null>(null);
  const anchor = useRef<HTMLSpanElement>(null);
  useEffect(() => {
    if (!toast || !message || anchor.current?.parentElement?.closest('[hidden]')) return;
    toastID.current = toast.notify(message, 'error');
    return () => {
      if (toastID.current !== null) toast.dismiss(toastID.current);
      toastID.current = null;
    };
  }, [message, toast?.dismiss, toast?.notify]);
  if (toast) return <span ref={anchor} hidden />;
  if (dismissed) return null;
  return <div {...props} className={`toast-error-notice ${className}`.trim()} role={role}>
    {message}
    <button type="button" className="icon-button" aria-label="关闭错误提示" onClick={() => setDismissed(true)}>×</button>
  </div>;
}

export function FloatingNotice({ children, className = '', role = 'status', ...props }: NoticeProps) {
  return <div {...props} className={`notice ${className}`.trim()} role={role}>{children}</div>;
}
