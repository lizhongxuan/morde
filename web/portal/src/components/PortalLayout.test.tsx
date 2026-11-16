import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { PortalLayout } from './PortalLayout';
import { ToastProvider } from '../app/toast';
import { portalApi } from '../app/api';
import { ApiError } from '../api/client';

const portalLayoutAuthState = vi.hoisted(() => ({ role: 'user' as string, signOut: vi.fn() }));
vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { name: 'Alice', role: portalLayoutAuthState.role }, signOut: portalLayoutAuthState.signOut }) }));

describe('PortalLayout', () => {
  beforeEach(() => { vi.spyOn(portalApi, 'taskApprovals').mockResolvedValue({ items: [], next_cursor: null, pending_count: 0 }); });
  afterEach(() => { cleanup(); vi.restoreAllMocks(); portalLayoutAuthState.role = 'user'; portalLayoutAuthState.signOut.mockReset(); localStorage.removeItem('morde:pc-context-sidebar-collapsed'); });

  function renderPortalLayout(path: string) {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(<QueryClientProvider client={client}><ToastProvider><MemoryRouter initialEntries={[path]}><PortalLayout /></MemoryRouter></ToastProvider></QueryClientProvider>);
  }

  it('exposes work and settings pages from the primary navigation', () => {
    renderPortalLayout('/assistant');

    expect(screen.getByRole('link', { name: '查看客户端与设备' })).toHaveAttribute('href', '/clients');
    expect(screen.queryByRole('link', { name: '打开手机端' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '返回上一页' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '前进' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '折叠侧边栏' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '工作台' })).not.toBeInTheDocument();
    const workNavigation = screen.getByRole('navigation', { name: '工作导航' });
    expect(within(workNavigation).getByRole('link', { name: /审核/ })).toHaveAttribute('href', '/reviews');
    expect(within(workNavigation).getByRole('link', { name: '云端电脑' })).toHaveAttribute('href', '/hosts');
    expect(within(workNavigation).queryByRole('link', { name: '本机项目' })).not.toBeInTheDocument();
    expect(within(workNavigation).getByRole('link', { name: 'Codex 会话' })).toHaveAttribute('href', '/codex');
    expect(within(workNavigation).getByRole('link', { name: '对话' })).toHaveAttribute('href', '/assistant');
    expect(within(workNavigation).getByRole('link', { name: '项目' })).toHaveAttribute('href', '/projects');
    expect(within(workNavigation).getByRole('link', { name: '个人数据' })).toHaveAttribute('href', '/personal');
    expect(within(workNavigation).getByRole('link', { name: '目标与计划' })).toHaveAttribute('href', '/goals');
    expect(within(workNavigation).getAllByRole('link', { name: '云端电脑' })).toHaveLength(1);
    const settingsNavigation = screen.getByRole('navigation', { name: '设置导航' });
    expect(within(settingsNavigation).getByRole('link', { name: '设置与授权' })).toHaveAttribute('href', '/connections');
    expect(within(settingsNavigation).queryByRole('link', { name: '文件同步' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '模型与工具' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'API Key' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '工作区' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '任务与计划' })).not.toBeInTheDocument();
  });

  it('preserves the review navigation during a network failure and offers retry in a popup', async () => {
    vi.mocked(portalApi.taskApprovals).mockResolvedValue({ items: [], next_cursor: null, pending_count: 3 });
    renderPortalLayout('/clients');
    expect(await screen.findByRole('link', { name: '审核，有 3 项待处理' })).toBeInTheDocument();

    cleanup();
    vi.mocked(portalApi.taskApprovals)
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce({ items: [], next_cursor: null, pending_count: 2 });
    renderPortalLayout('/clients');
    const reviewLink = await screen.findByRole('link', { name: '审核' });
    expect(reviewLink.querySelector('.morde-nav-count-error')).toBeNull();
    expect(await screen.findByRole('alert')).toHaveTextContent('审核状态暂时无法更新。');

    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    expect(await screen.findByRole('link', { name: '审核，有 2 项待处理' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('keeps the existing inline indicator for a non-network approval error', async () => {
    vi.mocked(portalApi.taskApprovals).mockRejectedValue(new ApiError({
      message: 'forbidden', code: 'forbidden', status: 403,
    }));
    renderPortalLayout('/assistant');
    const reviewLink = await screen.findByRole('link', { name: '审核状态暂不可用' });

    expect(reviewLink.querySelector('.morde-nav-count-error')).toHaveTextContent('!');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('groups setting pages under the categorized sidebar', () => {
    renderPortalLayout('/apikeys');
    const settingsNavigation = screen.getByRole('complementary', { name: '设置与授权子菜单' });
    expect(within(settingsNavigation).getByRole('link', { name: '模型与工具' })).toHaveAttribute('href', '/connections');
    expect(within(settingsNavigation).getByRole('link', { name: '客户端' })).toHaveAttribute('href', '/clients');
    expect(within(settingsNavigation).getByRole('link', { name: 'API Key' })).toHaveAttribute('aria-current', 'page');
    expect(within(settingsNavigation).getByRole('link', { name: '账户' })).toHaveAttribute('href', '/settings');
    expect(within(screen.getByRole('navigation', { name: '设置导航' })).getByRole('link', { name: '设置与授权' })).toHaveAttribute('href', '/connections');
  });

  it('places project sync under Cloud Computers in the PC navigation', () => {
    const { container } = renderPortalLayout('/hosts?sync=1');
    const contextNavigation = screen.getByRole('complementary', { name: '云端电脑子菜单' });
    expect(within(contextNavigation).getByRole('link', { name: '文件同步' })).toHaveAttribute('aria-current', 'page');
    expect(within(contextNavigation).getByRole('link', { name: '文件同步' })).toHaveAttribute('href', '/hosts?sync=1');
    expect(within(contextNavigation).getByRole('link', { name: '我的电脑' })).toHaveAttribute('href', '/hosts');
    expect(within(contextNavigation).getByRole('link', { name: '我的电脑' })).not.toHaveAttribute('aria-current', 'page');
    expect(within(screen.getByRole('navigation', { name: '设置导航' })).queryByRole('link', { name: '文件同步' })).not.toBeInTheDocument();
    expect(container.querySelector('.morde-top-actions > .notice-center')).toBeInTheDocument();
    expect(container.querySelector('.morde-main > .notice-center')).toBeNull();
  });

  it.each(['/notifications', '/projects'])('does not render a sidebar toggle without a context sidebar on %s', (path) => {
    const { container } = renderPortalLayout(path);
    expect(container.querySelector('#context-sidebar')).toBeNull();
    expect(screen.queryByRole('button', { name: /侧边栏/ })).not.toBeInTheDocument();
    expect(container.querySelector('.assistant-page')).toBeNull();
  });

  it('keeps settings and logout inside the account menu and confirms before signing out', () => {
    const { container } = renderPortalLayout('/assistant');
    const accountMenu = container.querySelector('details.morde-account-menu') as HTMLDetailsElement;
    expect(container.querySelector('.morde-sidebar-bottom .morde-nav-link[aria-label="设置与授权"]')).toBeInTheDocument();
    expect(accountMenu.querySelector('a[href="/settings"]')).toBeInTheDocument();
    expect(accountMenu.querySelector('.morde-profile-logout')).toBeInTheDocument();
    expect(accountMenu.open).toBe(false);

    fireEvent.click(accountMenu.querySelector('summary')!);
    expect(accountMenu.open).toBe(true);
    expect(screen.getByRole('link', { name: '账户设置' })).toHaveAttribute('href', '/settings');
    fireEvent.click(accountMenu.querySelector('.morde-profile-logout')!);
    const dialog = screen.getByRole('dialog', { name: '退出登录' });
    expect(portalLayoutAuthState.signOut).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole('button', { name: '退出登录' }));
    expect(portalLayoutAuthState.signOut).toHaveBeenCalledOnce();
  });

  it('keeps the administration entry inside the account menu', () => {
    portalLayoutAuthState.role = 'super_admin';
    const { container } = renderPortalLayout('/assistant');
    expect(screen.getByRole('link', { name: '切换至管理后台' })).not.toBeVisible();
    fireEvent.click(container.querySelector('.morde-account-menu > summary')!);
    expect(screen.getByRole('link', { name: '切换至管理后台' })).toHaveAttribute('href', '/admin/');
  });

  it('aligns page categories below the top bar and selects personal-data sections', () => {
    renderPortalLayout('/personal?section=soul');
    expect(screen.getByRole('complementary', { name: '个人数据子菜单' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'AI 个性' })).toHaveAttribute('aria-current', 'page');
    expect(screen.queryByRole('heading', { level: 1 })).not.toBeInTheDocument();
  });

  it('selects the default personal-data section and keeps the shared top bar above the sidebars', () => {
    const { container } = renderPortalLayout('/personal');
    expect(screen.getByRole('link', { name: '个人资料' })).toHaveAttribute('aria-current', 'page');
    const shell = container.querySelector('.morde-shell');
    expect(shell?.firstElementChild?.nextElementSibling).toHaveClass('morde-topbar');
    expect(shell?.querySelector(':scope > .page-navigation')).toBeInTheDocument();
  });

  it('collapses and expands the context sidebar from the top bar', () => {
    const { container } = renderPortalLayout('/assistant');
    const shell = container.querySelector('.morde-shell');
    expect(shell).not.toHaveClass('morde-shell-context-collapsed');

    fireEvent.click(screen.getByRole('button', { name: '折叠侧边栏' }));
    expect(screen.getByRole('button', { name: '展开侧边栏' })).toHaveAttribute('aria-expanded', 'false');
    expect(shell).toHaveClass('morde-shell-context-collapsed');

    fireEvent.click(screen.getByRole('button', { name: '展开侧边栏' }));
    expect(screen.getByRole('button', { name: '折叠侧边栏' })).toHaveAttribute('aria-expanded', 'true');
    expect(shell).not.toHaveClass('morde-shell-context-collapsed');
  });

  it('does not advertise projects to administrator accounts', () => {
    portalLayoutAuthState.role = 'super_admin';
    renderPortalLayout('/assistant');
    expect(screen.queryByRole('link', { name: '项目' })).not.toBeInTheDocument();
  });
});
