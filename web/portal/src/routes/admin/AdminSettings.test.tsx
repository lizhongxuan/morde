import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../../app/api';
import { AdminSettings } from './AdminSettings';

type Settings = Awaited<ReturnType<typeof portalApi.adminSettings>>;
const settings: Settings = {
  registration_review_required: true,
  automatic_new_user_model_budget: true,
  automatic_invitation_reward: false,
  automatic_contribution_reward: false,
  default_personal_data_quota_bytes: 5 * 1024 ** 3,
  default_personal_attachment_bytes: 300 * 1024 ** 2,
  default_model_budget: { configured: false, token_limit: 0, max_concurrent: 0, version: 0 },
  default_model: { configured: false, model_id: null, version: 0 },
};

type ModelConnection = Awaited<ReturnType<typeof portalApi.adminModelConnections>>['items'][number];
const sharedModel: ModelConnection = {
  id: 'p1234567', name: '平台文本工具模型', base_url: 'https://models.example.test/v1', model_name: 'morde-test',
  shared: true, has_api_key: true, capabilities: { text: true, tools: true, vision: false, text_stream: false },
  checked_at: '2026-10-22T08:00:00Z', created_at: '2026-10-20T08:00:00Z', updated_at: '2026-10-22T08:00:00Z',
};

function renderPage(modelItems: ModelConnection[] = []) {
  vi.spyOn(portalApi, 'adminModelConnections').mockResolvedValue({ items: modelItems, configuration_status: modelItems.length ? 'configured' : 'not_configured' });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><AdminSettings /></MemoryRouter></QueryClientProvider>);
}

describe('admin settings', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows the unconfigured approval gate and personal-data defaults', async () => {
    vi.spyOn(portalApi, 'adminSettings').mockResolvedValue(settings);
    renderPage();

    expect(await screen.findByText('需要审核')).toBeInTheDocument();
    expect(screen.getByText('首次通过时应用默认模型额度').closest('article')).toHaveTextContent('已开启');
    expect(screen.getByText(/尚未配置默认额度/)).toBeInTheDocument();
    expect(screen.queryByText('自动邀请奖励')).not.toBeInTheDocument();
    expect(screen.getByText('自动贡献奖励').closest('article')).toHaveTextContent('已关闭');
    expect(screen.getByText('5 GiB')).toBeInTheDocument();
    expect(screen.getByText('300 MiB')).toBeInTheDocument();
    expect(screen.getByLabelText('Token 额度')).toBeRequired();
    expect(screen.getByLabelText('最大并发数')).toHaveAttribute('max', '100');
  });

  it('saves a positive default budget with the current expected version', async () => {
    const input = userEvent.setup();
    const update = vi.spyOn(portalApi, 'updateAdminModelBudgetDefault').mockResolvedValue({
      configured: true, token_limit: 500_000, max_concurrent: 8, version: 1,
    });
    vi.spyOn(portalApi, 'adminSettings').mockResolvedValue(settings);
    renderPage();

    await screen.findByText('尚未配置默认额度。新账号仍可通过审核并使用个人模型；使用平台模型前需要配置额度。');
    await input.type(screen.getByLabelText('Token 额度'), '500000');
    await input.type(screen.getByLabelText('最大并发数'), '8');
    await input.click(screen.getByRole('button', { name: '保存默认额度' }));

    expect(update.mock.calls[0]?.[0]).toEqual({ expected_version: 0, token_limit: 500_000, max_concurrent: 8 });
    expect(await screen.findByText('默认模型额度已保存。')).toBeInTheDocument();
    expect(screen.getByText(/当前默认值：/)).toHaveTextContent('当前默认值：500,000 tokens · 并发 8 · 版本 v1');
  });

  it('requires refresh after a version conflict before allowing a retry', async () => {
    const input = userEvent.setup();
    const conflict = Object.assign(new Error('版本冲突'), { code: 'conflict', status: 409 });
    const update = vi.spyOn(portalApi, 'updateAdminModelBudgetDefault').mockRejectedValueOnce(conflict).mockResolvedValue({
      configured: true, token_limit: 700_000, max_concurrent: 5, version: 5,
    });
    const read = vi.spyOn(portalApi, 'adminSettings').mockResolvedValue({
      ...settings, default_model_budget: { configured: true, token_limit: 600_000, max_concurrent: 5, version: 4 },
    });
    renderPage();

    await input.clear(await screen.findByLabelText('Token 额度'));
    await input.type(screen.getByLabelText('Token 额度'), '700000');
    await input.click(screen.getByRole('button', { name: '保存默认额度' }));
    expect(await screen.findByText(/默认额度已被其他管理员修改/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: '保存默认额度' })).toBeDisabled();

    await input.click(screen.getByRole('button', { name: '刷新配置并放弃当前草稿' }));
    expect(await screen.findByDisplayValue('600000')).toBeInTheDocument();
    await input.clear(screen.getByLabelText('Token 额度'));
    await input.type(screen.getByLabelText('Token 额度'), '700000');
    await input.click(screen.getByRole('button', { name: '保存默认额度' }));
    expect(update).toHaveBeenCalledTimes(2);
    expect(await screen.findByText('默认模型额度已保存。')).toBeInTheDocument();
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('shows an error and retries the settings request', async () => {
    const request = vi.spyOn(portalApi, 'adminSettings').mockRejectedValueOnce(new Error('暂时不可用')).mockResolvedValue(settings);
    renderPage();

    expect(await screen.findByText('暂时不可用')).toBeInTheDocument();
    screen.getByRole('button', { name: '重试' }).click();
    expect(await screen.findByText('5 GiB')).toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(2);
  });

  it('configures the platform default from a checked shared text-and-tools model', async () => {
    const input = userEvent.setup();
    vi.spyOn(portalApi, 'adminSettings').mockResolvedValue(settings);
    const update = vi.spyOn(portalApi, 'updateAdminModelDefault').mockResolvedValue({ configured: true, model_id: sharedModel.id, version: 1 });
    renderPage([sharedModel]);

    expect(await screen.findByText('尚未配置默认模型。用户仍可选择已检查的个人模型或共享模型。')).toBeInTheDocument();
    await input.selectOptions(screen.getByLabelText('默认模型连接'), sharedModel.id);
    await input.click(screen.getByRole('button', { name: '保存默认模型' }));

    expect(update.mock.calls[0]?.[0]).toEqual({ expected_version: 0, model_id: sharedModel.id });
    expect(await screen.findByText('默认模型已保存。')).toBeInTheDocument();
    expect(screen.getByText(/当前默认模型：/)).toHaveTextContent('当前默认模型：平台文本工具模型 · 版本 v1');
  });

  it('only offers eligible shared models and lets the admin clear the default', async () => {
    const input = userEvent.setup();
    const current: Settings = { ...settings, default_model: { configured: true, model_id: sharedModel.id, version: 3 } };
    vi.spyOn(portalApi, 'adminSettings').mockResolvedValue(current);
    const update = vi.spyOn(portalApi, 'updateAdminModelDefault').mockResolvedValue({ configured: false, model_id: null, version: 4 });
    renderPage([
      sharedModel,
      { ...sharedModel, id: 'pnotchecked', checked_at: null },
      { ...sharedModel, id: 'pvision01', capabilities: { text: true, tools: false, vision: true, text_stream: false } },
      { ...sharedModel, id: 'ppersonal1', shared: false },
    ]);

    const select = await screen.findByLabelText('默认模型连接');
    expect(select.querySelectorAll('option')).toHaveLength(3);
    expect(select.querySelector('option[value="pnotchecked"]')).not.toBeInTheDocument();
    expect(select.querySelector('option[value="pvision01"]')).toBeInTheDocument();
    await input.selectOptions(select, '');
    await input.click(screen.getByRole('button', { name: '清除默认模型' }));

    expect(update.mock.calls[0]?.[0]).toEqual({ expected_version: 3, model_id: '' });
    expect(await screen.findByText('默认模型已保存。')).toBeInTheDocument();
  });
});
