import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter } from 'react-router';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { PortalLayout } from './PortalLayout';
import { ToastProvider } from '../app/toast';
import { portalApi } from '../app/api';

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

    expect(screen.queryByRole('link', { name: '查看客户端与设备' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '打开手机端' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: '返回上一页' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '前进' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '折叠侧边栏' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '工作台' })).not.toBeInTheDocument();
    const workNavigation = screen.getByRole('navigation', { name: '工作导航' });
    expect(within(workNavigation).getByRole('link', { name: /审核/ })).toHaveAttribute('href', '/reviews');
    expect(within(workNavigation).queryByRole('link', { name: '云端电脑' })).not.toBeInTheDocument();
    expect(within(workNavigation).queryByRole('link', { name: '本机项目' })).not.toBeInTheDocument();
    expect(within(workNavigation).queryByRole('link', { name: 'Codex 会话' })).not.toBeInTheDocument();
    expect(within(workNavigation).getByRole('link', { name: '对话' })).toHaveAttribute('href', '/assistant');
    expect(within(workNavigation).queryByRole('link', { name: '项目' })).not.toBeInTheDocument();
    expect(within(workNavigation).queryByRole('link', { name: '个人数据' })).not.toBeInTheDocument();
    expect(within(workNavigation).getByRole('link', { name: '目标' })).toHaveAttribute('href', '/goals?view=goals');
    expect(within(workNavigation).getByRole('link', { name: '定时计划' })).toHaveAttribute('href', '/goals?view=schedules');
    expect(screen.queryByRole('navigation', { name: '设置导航' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '模型与工具' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'API Key' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '工作区' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '任务与计划' })).not.toBeInTheDocument();
  });

  it('keeps workspace tabs closed by default and opens only the requested task tab', () => {
    renderPortalLayout('/assistant');

    expect(screen.getByRole('button', { name: '显示工作区侧栏' })).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByRole('navigation', { name: '对话工作区' })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: '显示工作区侧栏' }));

    const workspace = screen.getByRole('navigation', { name: '对话工作区' });
    const openTabs = within(workspace.querySelector('.morde-workspace-tab-list')!).getAllByRole('button');
    expect(openTabs).toHaveLength(2);
    expect(openTabs[0]).toHaveTextContent('任务动态');
    expect(openTabs.some((tab) => /云端电脑|文件/.test(tab.textContent ?? ''))).toBe(false);
  });

  it('shows the breathing network status while offline or disconnected from the API', () => {
    renderPortalLayout('/assistant');
    expect(screen.queryByText('连接中...')).not.toBeInTheDocument();

    fireEvent(window, new Event('offline'));
    expect(screen.getByText('连接中...').closest('[role="status"]')).toBeInTheDocument();
    expect(screen.getByText('连接中...')).toHaveClass('morde-network-connecting-label');

    fireEvent(window, new Event('online'));
    expect(screen.queryByText('连接中...')).not.toBeInTheDocument();

    fireEvent(window, new Event('morde:api-network-error'));
    expect(screen.getByText('连接中...').closest('[role="status"]')).toBeInTheDocument();
    fireEvent(window, new Event('morde:api-network-recovered'));
    expect(screen.queryByText('连接中...')).not.toBeInTheDocument();
  });

  it('does not fetch approval items or surface approval errors outside the review page', () => {
    renderPortalLayout('/assistant');

    expect(screen.getByRole('link', { name: '审核' })).toHaveAttribute('href', '/reviews');
    expect(portalApi.taskApprovals).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('groups setting pages in the settings sidebar', () => {
    renderPortalLayout('/hosts?view=apikeys');
    const settingsNavigation = screen.getByRole('navigation', { name: '设置与授权分类' });
    expect(screen.getByRole('complementary', { name: '设置与授权子菜单' })).toContainElement(settingsNavigation);
    expect(within(settingsNavigation).getByText('连接与设备')).toBeInTheDocument();
    expect(within(settingsNavigation).getByText('个人设置')).toBeInTheDocument();
    expect(within(settingsNavigation).getByRole('link', { name: '模型与工具' })).toHaveAttribute('href', '/connections');
    expect(within(settingsNavigation).getByRole('link', { name: '文件同步' })).toHaveAttribute('href', '/hosts?sync=1');
    expect(within(settingsNavigation).queryByRole('link', { name: '客户端' })).not.toBeInTheDocument();
    expect(within(settingsNavigation).getByRole('link', { name: '云端电脑' })).toHaveAttribute('aria-current', 'page');
    expect(within(settingsNavigation).queryByRole('link', { name: 'API Key' })).not.toBeInTheDocument();
    expect(within(settingsNavigation).getByRole('link', { name: '账户' })).toHaveAttribute('href', '/settings');
    for (const [section, label] of [['profile', '个人资料'], ['soul', 'AI 个性'], ['memory', '记忆'], ['library', '资源库']]) {
      expect(within(settingsNavigation).getByRole('link', { name: label })).toHaveAttribute('href', `/personal?section=${section}`);
    }
    for (const label of ['动态', '灵感', '偏好']) expect(within(settingsNavigation).queryByRole('link', { name: label })).not.toBeInTheDocument();
    expect(within(settingsNavigation).queryByRole('link', { name: '个人数据' })).not.toBeInTheDocument();
    expect(screen.queryByRole('navigation', { name: '设置导航' })).not.toBeInTheDocument();
  });

  it('places project sync under Cloud Computers in the PC navigation', () => {
    const { container } = renderPortalLayout('/hosts?sync=1');
    const contextNavigation = screen.getByRole('navigation', { name: '设置与授权分类' });
    expect(screen.getByRole('complementary', { name: '设置与授权子菜单' })).toContainElement(contextNavigation);
    expect(within(contextNavigation).getByRole('link', { name: '文件同步' })).toHaveAttribute('aria-current', 'page');
    expect(within(contextNavigation).getByRole('link', { name: '文件同步' })).toHaveAttribute('href', '/hosts?sync=1');
    expect(within(contextNavigation).getByRole('link', { name: '云端电脑' })).toHaveAttribute('href', '/hosts');
    expect(within(contextNavigation).getByRole('link', { name: '云端电脑' })).not.toHaveAttribute('aria-current', 'page');
    expect(screen.queryByRole('navigation', { name: '设置导航' })).not.toBeInTheDocument();
    expect(container.querySelector('.morde-top-actions > .notice-center')).toBeInTheDocument();
    expect(container.querySelector('.morde-main > .notice-center')).toBeNull();
  });

  it.each(['/notifications', '/projects'])('does not render a sidebar toggle without a context sidebar on %s', (path) => {
    const { container } = renderPortalLayout(path);
    expect(container.querySelector('#context-sidebar')).toBeNull();
    expect(screen.queryByRole('button', { name: /侧边栏/ })).not.toBeInTheDocument();
    expect(container.querySelector('.assistant-page')).toBeNull();
  });

  it.each([
    ['/goals', '目标', '定时计划'],
    ['/goals?view=goals', '目标', '定时计划'],
    ['/goals?view=schedules', '定时计划', '目标'],
    ['/goals?view=unknown', '目标', '定时计划'],
  ])('selects %s in the main navigation without a context sidebar', (path, activeLabel, inactiveLabel) => {
    const { container } = renderPortalLayout(path);
    const workNavigation = screen.getByRole('navigation', { name: '工作导航' });
    expect(within(workNavigation).getByRole('link', { name: '目标' })).toHaveAttribute('href', '/goals?view=goals');
    expect(within(workNavigation).getByRole('link', { name: '定时计划' })).toHaveAttribute('href', '/goals?view=schedules');
    expect(within(workNavigation).queryByRole('link', { name: '目标与计划' })).not.toBeInTheDocument();
    expect(within(workNavigation).getByRole('link', { name: activeLabel })).toHaveAttribute('aria-current', 'page');
    expect(within(workNavigation).getByRole('link', { name: activeLabel })).toHaveClass('active');
    expect(within(workNavigation).getByRole('link', { name: inactiveLabel })).not.toHaveAttribute('aria-current');
    expect(within(workNavigation).getByRole('link', { name: inactiveLabel })).not.toHaveClass('active');
    expect(container.querySelector('#context-sidebar')).toBeNull();
    expect(screen.queryByRole('button', { name: /侧边栏/ })).not.toBeInTheDocument();
  });

  it('keeps settings and logout inside the account menu and confirms before signing out', () => {
    const { container } = renderPortalLayout('/assistant');
    const accountMenu = container.querySelector('details.morde-account-menu') as HTMLDetailsElement;
    expect(container.querySelector('.morde-sidebar-bottom .morde-nav-link')).not.toBeInTheDocument();
    expect(accountMenu.querySelector('.morde-account-menu-action[href="/connections"]')).toHaveTextContent('设置');
    expect(accountMenu.querySelector('a[href="/settings"]')).not.toBeInTheDocument();
    expect(accountMenu.querySelector('.morde-profile-logout')).toBeInTheDocument();
    expect(accountMenu.open).toBe(false);

    fireEvent.click(accountMenu.querySelector('summary')!);
    expect(accountMenu.open).toBe(true);
    expect(screen.getByRole('link', { name: '设置' })).toHaveAttribute('href', '/connections');
    expect(screen.queryByRole('link', { name: '账户设置' })).not.toBeInTheDocument();
    fireEvent.pointerDown(accountMenu.querySelector('strong')!);
    expect(accountMenu.open).toBe(true);
    fireEvent.pointerDown(document.body);
    expect(accountMenu.open).toBe(false);
    fireEvent.click(accountMenu.querySelector('summary')!);
    expect(accountMenu.open).toBe(true);
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
    const settingsNavigation = screen.getByRole('navigation', { name: '设置与授权分类' });
    expect(screen.getByRole('complementary', { name: '设置与授权子菜单' })).toContainElement(settingsNavigation);
    expect(within(settingsNavigation).getByRole('link', { name: 'AI 个性' })).toHaveAttribute('aria-current', 'page');
    expect(within(settingsNavigation).getByRole('link', { name: '个人资料' })).toHaveAttribute('href', '/personal?section=profile');
    for (const label of ['动态', '灵感', '偏好']) expect(within(settingsNavigation).queryByRole('link', { name: label })).not.toBeInTheDocument();
    expect(within(settingsNavigation).queryByRole('link', { name: '个人数据' })).not.toBeInTheDocument();
    expect(screen.queryByRole('navigation', { name: '设置导航' })).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { level: 1 })).not.toBeInTheDocument();
  });

  it('selects the default personal-data section and keeps the shared top bar above the sidebars', () => {
    const { container } = renderPortalLayout('/personal');
    expect(screen.getByRole('link', { name: '个人资料' })).toHaveAttribute('aria-current', 'page');
    const shell = container.querySelector('.morde-shell');
    expect(shell?.firstElementChild?.nextElementSibling).toHaveClass('morde-topbar');
    expect(shell?.querySelector(':scope > .page-navigation')).toBeInTheDocument();
    expect(shell?.querySelector(':scope > .morde-main > .settings-navigation')).not.toBeInTheDocument();
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
