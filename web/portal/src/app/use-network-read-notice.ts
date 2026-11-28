import { useCallback, useEffect, useRef } from 'react';
import { isTransientNetworkError } from '../api/errors';
import { useToast } from './toast';

type NetworkReadFailure = {
  failed: boolean;
  error: unknown;
  retry: () => unknown;
};

export function useNetworkReadNotice(failures: readonly NetworkReadFailure[], message = '网络异常，部分数据暂时无法读取。') {
  const { notify, dismiss } = useToast();
  const failuresRef = useRef(failures);
  const hasNetworkFailure = failures.some((failure) => failure.failed && isTransientNetworkError(failure.error));
  const noticeID = useRef<number | null>(null);
  const previouslyHadNetworkFailure = useRef(false);
  const retry = useCallback(() => {
    for (const failure of failuresRef.current) {
      if (failure.failed && isTransientNetworkError(failure.error)) void failure.retry();
    }
  }, []);

  useEffect(() => { failuresRef.current = failures; }, [failures]);

  useEffect(() => {
    if (!hasNetworkFailure) {
      if (previouslyHadNetworkFailure.current) window.dispatchEvent(new CustomEvent('morde:api-network-recovered', { detail: { source: 'read' } }));
      previouslyHadNetworkFailure.current = false;
      if (noticeID.current !== null) dismiss(noticeID.current);
      noticeID.current = null;
    } else if (noticeID.current === null) {
      previouslyHadNetworkFailure.current = true;
      window.dispatchEvent(new CustomEvent('morde:api-network-error', { detail: { source: 'read' } }));
      noticeID.current = notify(message, 'error', { label: '重试', onClick: retry });
    } else {
      previouslyHadNetworkFailure.current = true;
    }
  }, [dismiss, hasNetworkFailure, message, notify, retry]);

  useEffect(() => () => {
    if (previouslyHadNetworkFailure.current) window.dispatchEvent(new CustomEvent('morde:api-network-recovered', { detail: { source: 'read' } }));
    if (noticeID.current !== null) dismiss(noticeID.current);
  }, [dismiss]);

  return hasNetworkFailure;
}
