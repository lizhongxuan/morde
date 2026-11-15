import { useState, type HTMLAttributes, type ReactNode } from 'react';

type NoticeProps = HTMLAttributes<HTMLDivElement> & { children: ReactNode };

export function FloatingError({ children, className = '', role = 'alert', ...props }: NoticeProps) {
  const [dismissed, setDismissed] = useState(false);
  if (dismissed) return null;
  return <div {...props} className={`toast-error-notice ${className}`.trim()} role={role}>
    {children}
    <button type="button" className="icon-button" aria-label="关闭错误提示" onClick={() => setDismissed(true)}>×</button>
  </div>;
}

export function FloatingNotice({ children, className = '', role = 'status', ...props }: NoticeProps) {
  return <div {...props} className={`notice ${className}`.trim()} role={role}>{children}</div>;
}
