import { useState, type HTMLAttributes, type ReactNode } from 'react';

export default function FloatingError({ children, className = '', role = 'alert', ...props }: HTMLAttributes<HTMLDivElement> & { children: ReactNode }) {
  const [dismissed, setDismissed] = useState(false);
  if (dismissed) return null;
  return <div {...props} className={`workbench-floating-error ${className}`.trim()} role={role}>
    <span>{children}</span>
    <button type="button" className="icon-button" aria-label={role === 'alert' ? '关闭错误提示' : '关闭状态提示'} onClick={() => setDismissed(true)}>×</button>
  </div>;
}
