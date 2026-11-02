import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../../app/api';
import { AdminSettings } from './AdminSettings';

type Settings = Awaited<ReturnType<typeof portalApi.adminSettings>>;
const settings: Settings = {
  registration_review_required: true,
  automatic_new_user_model_budget: false,
  automatic_invitation_reward: false,
  automatic_contribution_reward: false,
  default_personal_data_quota_bytes: 5 * 1024 ** 3,
  default_personal_attachment_bytes: 300 * 1024 ** 2,
};

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><AdminSettings /></MemoryRouter></QueryClientProvider>);
}

describe('admin settings', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows fixed policy states and personal-data defaults as read-only', async () => {
    vi.spyOn(portalApi, 'adminSettings').mockResolvedValue(settings);
    renderPage();

    expect(await screen.findByText('需要审核')).toBeInTheDocument();
    expect(screen.getByText('新用户自动模型预算').closest('article')).toHaveTextContent('已关闭');
    expect(screen.getByText('自动邀请奖励').closest('article')).toHaveTextContent('已关闭');
    expect(screen.getByText('自动贡献奖励').closest('article')).toHaveTextContent('已关闭');
    expect(screen.getByText('5 GiB')).toBeInTheDocument();
    expect(screen.getByText('300 MiB')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /查看用户与按人设置模型预算/ })).toHaveAttribute('href', '/admin/users');
    expect(screen.queryByRole('button', { name: /修改|保存/ })).not.toBeInTheDocument();
  });

  it('shows an error and retries the settings request', async () => {
    const request = vi.spyOn(portalApi, 'adminSettings').mockRejectedValueOnce(new Error('暂时不可用')).mockResolvedValue(settings);
    renderPage();

    expect(await screen.findByText('暂时不可用')).toBeInTheDocument();
    screen.getByRole('button', { name: '重试' }).click();
    expect(await screen.findByText('5 GiB')).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(2);
  });
});
