import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import FloatingError from '../components/FloatingError';

type NoticeAction = { label: string; onClick: () => void };
type Notice = { id: number; message: string; action?: NoticeAction };
type NoticeContextValue = { notify: (message: string, action?: NoticeAction) => void };
const NoticeContext = createContext<NoticeContextValue>({ notify: () => undefined });

export function WorkbenchNoticeProvider({ children }: { children: ReactNode }) {
  const [notice, setNotice] = useState<Notice | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const nextID = useRef(0);
  const dismiss = useCallback(() => {
    if (timer.current !== undefined) window.clearTimeout(timer.current);
    timer.current = undefined;
    setNotice(null);
  }, []);
  useEffect(() => () => {
    if (timer.current !== undefined) window.clearTimeout(timer.current);
  }, []);
  const notify = useCallback((message: string, action?: NoticeAction) => {
    setNotice({ id: ++nextID.current, message, action });
    if (timer.current !== undefined) window.clearTimeout(timer.current);
    timer.current = window.setTimeout(dismiss, 6000);
  }, [dismiss]);

  return <NoticeContext.Provider value={{ notify }}>
    {children}
    {notice && <div className="workbench-notice-region workbench-network-notice-region" aria-label="网络错误提示" aria-live="assertive">
      <FloatingError key={notice.id}>
        {notice.message}{notice.action && <button className="button button-small button-secondary" type="button" onClick={() => {
          const action = notice.action?.onClick;
          dismiss();
          action?.();
        }}>{notice.action.label}</button>}
      </FloatingError>
    </div>}
  </NoticeContext.Provider>;
}

export function useWorkbenchNotice() {
  return useContext(NoticeContext).notify;
}
