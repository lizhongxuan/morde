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
      sample_count: 0, failed_tasks: 2, failed_hub_operations: 1, unknown_hub_operations: 0,
    });
    renderPage();

    expect(await screen.findByRole('heading', { name: '质量与风险统计' })).toBeInTheDocument();
    expect(await screen.findByText('当前没有经同意的质量样本。')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('当前不可用');
    expect(screen.getByText(/普通会话正文不会作为替代数据/)).toBeInTheDocument();
    expect(screen.getByText('失败 Task').closest('article')).toHaveTextContent('2');
    expect(screen.queryByText(/评分|高质量账户/)).not.toBeInTheDocument();
  });

  it('shows only source availability when available, and requests the selected period', async () => {
    const user = userEvent.setup();
    const request = vi.spyOn(portalApi, 'adminQuality').mockImplementation(async (days) => ({
      days: days ?? 7, available: true, sample_count: 3, failed_tasks: 1, failed_hub_operations: 2, unknown_hub_operations: 1,
    }));
    renderPage();

    expect(await screen.findByText(/已获准样本只统计数量；接口未计算质量评分或读取样本正文/)).toBeInTheDocument();
    expect(screen.getByText('当前授权训练样本').closest('article')).toHaveTextContent('3');
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
    resolveRequest({ days: 7, available: false, unavailable_reason: '未启用。', sample_count: 0,
      failed_tasks: 0, failed_hub_operations: 0, unknown_hub_operations: 0 });
    expect(await screen.findByText('未启用。')).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(2);
  });
});
