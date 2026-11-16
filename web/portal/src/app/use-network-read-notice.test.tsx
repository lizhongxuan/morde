import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ToastNoticeCenter, ToastProvider } from './toast';
import { useNetworkReadNotice } from './use-network-read-notice';

function NetworkRead({ failed, error, retry }: { failed: boolean; error: unknown; retry: () => unknown }) {
  useNetworkReadNotice([{ failed, error, retry }], '网络读取失败');
  return <main><p>原页面内容</p></main>;
}

describe('useNetworkReadNotice', () => {
  afterEach(cleanup);

  it('shows a popup for network read failures and retries the original request', async () => {
    const retry = vi.fn();
    const view = render(<ToastProvider><ToastNoticeCenter autoOpenErrors />
      <NetworkRead failed error={new TypeError('Failed to fetch')} retry={retry} />
    </ToastProvider>);

    const notice = await screen.findByRole('alert');
    expect(notice).toHaveTextContent('网络读取失败');
    expect(screen.getByText('原页面内容')).toBeInTheDocument();
    expect(notice.closest('main')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(retry).toHaveBeenCalledOnce();

    view.rerender(<ToastProvider><ToastNoticeCenter autoOpenErrors />
      <NetworkRead failed={false} error={undefined} retry={retry} />
    </ToastProvider>);
    await waitFor(() => expect(screen.queryByRole('alert')).not.toBeInTheDocument());
  });
});
