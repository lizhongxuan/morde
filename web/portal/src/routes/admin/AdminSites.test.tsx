import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router';
import { portalApi } from '../../app/api';
import { AdminSites } from './AdminSites';

type SitesPage = Awaited<ReturnType<typeof portalApi.adminSites>>;
type NudgePolicy = Awaited<ReturnType<typeof portalApi.adminNudges>>;
const sites: SitesPage = {
  items: [{ id: 'site-local-1', owner_id: 'u-owner', owner_name: 'Alice', host_id: 'h-local-1', state: 'active', created_at: '2026-10-31T10:00:00Z', release_count: 3, healthy_release_count: 2, latest_release_at: '2026-10-31T10:00:00Z' }],
  summary: { sites: 1, active_sites: 1, releases: 3, healthy_releases: 2 },
  integrations: { demo: 'not_configured', github_stats: 'not_configured', traffic: 'not_collected' },
  total: 1, limit: 25, offset: 0,
};
const initialPolicy: NudgePolicy = { enabled: false, url: '', text_zh: '', text_en: '', version: 1, updated_at: null };

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><MemoryRouter><AdminSites /></MemoryRouter></QueryClientProvider>);
}

describe('admin sites and operational nudge', () => {
  afterEach(() => { cleanup(); vi.restoreAllMocks(); });

  it('shows local metadata and genuine unconfigured integration states', async () => {
    vi.spyOn(portalApi, 'adminSites').mockResolvedValue(sites);
    vi.spyOn(portalApi, 'adminNudges').mockResolvedValue(initialPolicy);
    renderPage();

    expect(await screen.findByText('site-local-1')).toBeInTheDocument();
    expect(screen.getByText('2 / 3 健康')).toBeInTheDocument();
    expect(screen.getAllByText('未配置')).toHaveLength(2);
    expect(screen.getByText('未采集')).toBeInTheDocument();
    expect(screen.getByText(/不展示或发布站点内容/)).toBeInTheDocument();
    expect(screen.getByText('当前状态：已关闭 · 版本 v1 · 最近更新 —')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /发布|预览/ })).not.toBeInTheDocument();
  });

  it('validates an enabled nudge and sends the policy version on save', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'adminSites').mockResolvedValue({ ...sites, items: [], summary: { sites: 0, active_sites: 0, releases: 0, healthy_releases: 0 }, total: 0 });
    vi.spyOn(portalApi, 'adminNudges').mockResolvedValue(initialPolicy);
    const update = vi.spyOn(portalApi, 'updateAdminNudges').mockResolvedValue({ enabled: true, url: 'https://github.com/example/project', text_zh: '欢迎支持', text_en: 'Support the project', version: 2, updated_at: '2026-11-02T10:00:00Z' });
    renderPage();

    await user.click(await screen.findByRole('checkbox', { name: '启用运营提示' }));
    await user.type(screen.getByLabelText('GitHub 项目链接'), 'https://example.com/project');
    await user.type(screen.getByLabelText('中文提示'), '欢迎支持');
    await user.type(screen.getByLabelText('English message'), 'Support the project');
    await user.click(screen.getByRole('button', { name: '保存运营提示' }));
    expect(await screen.findByText(/链接必须是指向 GitHub 项目的 HTTPS 地址/)).toBeInTheDocument();
    expect(update).not.toHaveBeenCalled();

    await user.clear(screen.getByLabelText('GitHub 项目链接'));
    await user.type(screen.getByLabelText('GitHub 项目链接'), 'https://github.com/example/project');
    await user.click(screen.getByRole('button', { name: '保存运营提示' }));
    expect(await screen.findByText('运营提示策略已保存。')).toBeInTheDocument();
    expect(update).toHaveBeenCalledWith({ expected_version: 1, enabled: true, url: 'https://github.com/example/project', text_zh: '欢迎支持', text_en: 'Support the project' });
  });

  it('paginates the local site metadata list', async () => {
    const user = userEvent.setup();
    const otherSite = { ...sites.items[0]!, id: 'site-local-2', owner_id: 'u-owner-2', host_id: 'h-local-2' };
    const request = vi.spyOn(portalApi, 'adminSites').mockImplementation(async (_limit, offset) => offset
      ? { ...sites, items: [otherSite], total: 26, offset }
      : { ...sites, total: 26 });
    vi.spyOn(portalApi, 'adminNudges').mockResolvedValue(initialPolicy);
    renderPage();

    expect(await screen.findByText('site-local-1')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: '下一页站点' }));
    expect(await screen.findByText('site-local-2')).toBeInTheDocument();
    expect(request).toHaveBeenLastCalledWith(25, 25);
  });

  it('stops edits after a version conflict until the latest policy is reloaded', async () => {
    const user = userEvent.setup();
    vi.spyOn(portalApi, 'adminSites').mockResolvedValue({ ...sites, items: [], summary: { sites: 0, active_sites: 0, releases: 0, healthy_releases: 0 }, total: 0 });
    let latestPolicy = initialPolicy;
    vi.spyOn(portalApi, 'adminNudges').mockImplementation(async () => latestPolicy);
    const update = vi.spyOn(portalApi, 'updateAdminNudges').mockRejectedValueOnce(Object.assign(new Error('版本冲突'), { status: 409, code: 'conflict' }));
    renderPage();

    await user.type(await screen.findByLabelText('GitHub 项目链接'), 'https://github.com/example/project');
    await user.type(screen.getByLabelText('中文提示'), '欢迎');
    await user.type(screen.getByLabelText('English message'), 'Welcome');
    await user.click(screen.getByRole('button', { name: '保存运营提示' }));
    expect(await screen.findByText(/运营提示版本已变化/)).toBeInTheDocument();
    expect(update).toHaveBeenCalledWith({ expected_version: 1, enabled: false, url: 'https://github.com/example/project', text_zh: '欢迎', text_en: 'Welcome' });

    latestPolicy = { ...initialPolicy, version: 2, updated_at: '2026-11-02T10:00:00Z' };
    await user.click(screen.getByRole('button', { name: '重新加载' }));
    expect(await screen.findByText(/当前状态：已关闭 · 版本 v2/)).toBeInTheDocument();
    expect(screen.queryByText(/运营提示版本已变化/)).not.toBeInTheDocument();
  });
});
