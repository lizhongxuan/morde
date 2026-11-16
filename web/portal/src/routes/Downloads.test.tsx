import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { getDesktopDownloads } from '../app/desktopDownloads';
import { ToastNoticeCenter, ToastProvider } from '../app/toast';
import { Downloads } from './Downloads';

vi.mock('../app/desktopDownloads', () => ({
  desktopCapabilityNames: {},
  getDesktopDownloads: vi.fn(),
}));

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><ToastProvider><ToastNoticeCenter autoOpenErrors overlay /><MemoryRouter><Downloads /></MemoryRouter></ToastProvider></QueryClientProvider>);
}

describe('desktop downloads page', () => {
  afterEach(() => { cleanup(); vi.clearAllMocks(); });

  it('keeps the page recoverable after a network error and uses the Morde logo', async () => {
    vi.mocked(getDesktopDownloads)
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce({ version: null, artifacts: [] });
    renderPage();

    const notice = await screen.findByRole('alert');
    expect(notice).toHaveTextContent('网络异常，可用版本暂时无法读取。');
    expect(notice.closest('.download-page')).toBeNull();
    expect(screen.getByRole('heading', { name: '下载 morde-pc' })).toBeInTheDocument();
    expect(document.querySelector('.download-topbar .morde-brand-mark')).toBeInTheDocument();
    await userEvent.setup().click(screen.getByRole('button', { name: '重试' }));

    expect(await screen.findByRole('heading', { name: '当前还没有可下载版本' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: '下载 morde-pc' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(getDesktopDownloads).toHaveBeenCalledTimes(2);
  });
});
