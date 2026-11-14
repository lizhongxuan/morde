import { useState, type HTMLAttributes, type ReactNode } from 'react';

export default function FloatingError({ children, ...props }: HTMLAttributes<HTMLDivElement> & { children: ReactNode }) {
  const [dismissed, setDismissed] = useState(false);
  if (dismissed) return null;
  return <div {...props} className="workbench-floating-error" role="alert">
    <span>{children}</span>
    <button type="button" className="icon-button" aria-label="关闭错误提示" onClick={() => setDismissed(true)}>×</button>
  </div>;
}
