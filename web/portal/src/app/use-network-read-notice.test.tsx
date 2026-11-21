import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ToastNoticeCenter, ToastProvider } from './toast';
import { useNetworkReadNotice } from './use-network-read-notice';

function NetworkRead({ failed, error, retry }: { failed: boolean; error: unknown; retry: () => unknown }) {
  useNetworkReadNotice([{ failed, error, retry }], '网络读取失败');
  return <main><p>原页面内容</p></main>;
}

describe('useNetworkReadNotice', () => {
  afterEach(cleanup);

  it('shows one top-left error without a retry button and dismisses it after recovery', async () => {
    const retry = vi.fn();
    const view = render(<ToastProvider><ToastNoticeCenter autoOpenErrors />
      <NetworkRead failed error={new TypeError('Failed to fetch')} retry={retry} />
    </ToastProvider>);

    const notice = await screen.findByRole('alert');
    expect(notice).toHaveTextContent('网络读取失败');
    expect(screen.getByText('原页面内容')).toBeInTheDocument();
    expect(notice.closest('.error-notice-overlay')).toBeInTheDocument();
    expect(notice.closest('main')).toBeNull();
    expect(screen.queryByRole('button', { name: '重试' })).not.toBeInTheDocument();
    expect(retry).not.toHaveBeenCalled();

    view.rerender(<ToastProvider><ToastNoticeCenter autoOpenErrors />
      <NetworkRead failed={false} error={undefined} retry={retry} />
    </ToastProvider>);
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });
});
