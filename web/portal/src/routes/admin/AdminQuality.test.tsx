import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { portalApi } from '../../app/api';
import { AdminQuality } from './AdminQuality';

type QualityData = Awaited<ReturnType<typeof portalApi.adminQuality>>;

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><AdminQuality /></QueryClientProvider>);
}

describe('admin quality and risk', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows the source denial and does not invent quality metrics', async () => {
    vi.spyOn(portalApi, 'adminQuality').mockResolvedValue({
      days: 7, available: false, unavailable_reason: '当前没有经同意的质量样本。',
    });
    renderPage();

    expect(await screen.findByRole('heading', { name: '质量与风险' })).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('当前不可用');
    expect(screen.getByRole('status')).toHaveTextContent('当前没有经同意的质量样本。');
    expect(screen.getByText(/普通会话正文不会作为替代数据/)).toBeInTheDocument();
    expect(screen.queryByText(/得分|风险数|高质量/)).not.toBeInTheDocument();
  });

  it('shows only source availability when available, and requests the selected period', async () => {
    const user = userEvent.setup();
    const request = vi.spyOn(portalApi, 'adminQuality').mockImplementation(async (days) => ({ days, available: true }));
    renderPage();

    expect(await screen.findByText('可用', { exact: true })).toBeInTheDocument();
    expect(screen.getByText(/未返回指标明细/)).toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText('统计区间'), '30');
    expect(await screen.findByText('统计区间：近 30 天')).toBeInTheDocument();
    expect(request).toHaveBeenCalledWith(7);
    expect(request).toHaveBeenCalledWith(30);
  });

  it('shows loading, then reports and retries request errors', async () => {
    const user = userEvent.setup();
    let resolveRequest!: (value: QualityData) => void;
    const request = vi.spyOn(portalApi, 'adminQuality')
      .mockRejectedValueOnce(new Error('质量数据暂不可用'))
      .mockImplementationOnce(() => new Promise((resolve) => { resolveRequest = resolve; }));
    renderPage();

    expect(await screen.findByRole('alert')).toHaveTextContent('质量数据暂不可用');
    await user.click(screen.getByRole('button', { name: '重试' }));
    resolveRequest({ days: 7, available: false, unavailable_reason: '未启用。' });
    expect(await screen.findByText('未启用。')).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(2);
  });
});
