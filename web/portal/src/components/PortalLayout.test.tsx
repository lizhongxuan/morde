import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { PortalLayout } from './PortalLayout';
import { ToastProvider } from '../app/toast';

const portalLayoutAuthState = vi.hoisted(() => ({ role: 'user' as string, signOut: vi.fn() }));
vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { name: 'Alice', role: portalLayoutAuthState.role }, signOut: portalLayoutAuthState.signOut }) }));

describe('PortalLayout', () => {
  afterEach(() => { cleanup(); portalLayoutAuthState.role = 'user'; portalLayoutAuthState.signOut.mockReset(); localStorage.removeItem('morde:pc-context-sidebar-collapsed'); });

  it('exposes work and settings pages from the primary navigation', () => {
    render(<ToastProvider><MemoryRouter initialEntries={['/assistant']}><PortalLayout /></MemoryRouter></ToastProvider>);

    expect(screen.getByRole('link', { name: '打开手机端' })).toHaveAttribute('href', '/app/');
    expect(screen.getByRole('button', { name: '返回上一页' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '前进' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '折叠侧边栏' })).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '工作台' })).not.toBeInTheDocument();
    const workNavigation = screen.getByRole('navigation', { name: '工作导航' });
    expect(within(workNavigation).getByRole('link', { name: '云端电脑' })).toHaveAttribute('href', '/hosts');
    expect(within(workNavigation).getByRole('link', { name: '对话' })).toHaveAttribute('href', '/assistant');
    expect(within(workNavigation).getByRole('link', { name: '项目' })).toHaveAttribute('href', '/projects');
    expect(within(workNavigation).getByRole('link', { name: '个人数据' })).toHaveAttribute('href', '/personal');
    expect(within(workNavigation).getByRole('link', { name: '目标与计划' })).toHaveAttribute('href', '/goals');
    expect(within(workNavigation).getAllByRole('link', { name: '云端电脑' })).toHaveLength(1);
    const settingsNavigation = screen.getByRole('navigation', { name: '设置导航' });
    expect(within(settingsNavigation).getByRole('link', { name: '模型与工具' })).toHaveAttribute('href', '/connections');
    expect(within(settingsNavigation).getByRole('link', { name: '文件同步' })).toHaveAttribute('href', '/workspaces');
    expect(within(settingsNavigation).getByRole('link', { name: '客户端' })).toHaveAttribute('href', '/clients');
    expect(within(settingsNavigation).getByRole('link', { name: 'API Key' })).toHaveAttribute('href', '/apikeys');
    expect(within(settingsNavigation).getByRole('link', { name: '账户' })).toHaveAttribute('href', '/settings');
    expect(screen.getByRole('link', { name: '账户' })).toHaveAttribute('href', '/settings');
    expect(screen.queryByRole('link', { name: '工作区' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '任务与计划' })).not.toBeInTheDocument();
  });

  it.each(['/notifications', '/hosts', '/projects', '/workspaces', '/connections', '/clients', '/apikeys', '/settings'])('does not render a sidebar toggle without a context sidebar on %s', (path) => {
    const { container } = render(<ToastProvider><MemoryRouter initialEntries={[path]}><PortalLayout /></MemoryRouter></ToastProvider>);
    expect(container.querySelector('#context-sidebar')).toBeNull();
    expect(screen.queryByRole('button', { name: /侧边栏/ })).not.toBeInTheDocument();
    expect(container.querySelector('.assistant-page')).toBeNull();
  });

  it('keeps settings and logout inside the account menu and confirms before signing out', () => {
    const { container } = render(<ToastProvider><MemoryRouter initialEntries={['/assistant']}><PortalLayout /></MemoryRouter></ToastProvider>);
    const accountMenu = container.querySelector('details.morde-account-menu') as HTMLDetailsElement;
    expect(container.querySelector('.morde-sidebar-bottom > .morde-nav-link')).toBeNull();
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
    const { container } = render(<ToastProvider><MemoryRouter initialEntries={['/assistant']}><PortalLayout /></MemoryRouter></ToastProvider>);
    expect(screen.getByRole('link', { name: '切换至管理后台' })).not.toBeVisible();
    fireEvent.click(container.querySelector('.morde-account-menu > summary')!);
    expect(screen.getByRole('link', { name: '切换至管理后台' })).toHaveAttribute('href', '/admin/');
  });

  it('aligns page categories below the top bar and selects personal-data sections', () => {
    render(<ToastProvider><MemoryRouter initialEntries={['/personal?section=soul']}><PortalLayout /></MemoryRouter></ToastProvider>);
    expect(screen.getByRole('complementary', { name: '个人数据子菜单' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'AI 个性' })).toHaveAttribute('aria-current', 'page');
    expect(screen.queryByRole('heading', { level: 1 })).not.toBeInTheDocument();
  });

  it('selects the default personal-data section and keeps the shared top bar above the sidebars', () => {
    const { container } = render(<ToastProvider><MemoryRouter initialEntries={['/personal']}><PortalLayout /></MemoryRouter></ToastProvider>);
    expect(screen.getByRole('link', { name: '个人资料' })).toHaveAttribute('aria-current', 'page');
    const shell = container.querySelector('.morde-shell');
    expect(shell?.firstElementChild?.nextElementSibling).toHaveClass('morde-topbar');
    expect(shell?.querySelector(':scope > .page-navigation')).toBeInTheDocument();
  });

  it('collapses and expands the context sidebar from the top bar', () => {
    const { container } = render(<ToastProvider><MemoryRouter initialEntries={['/assistant']}><PortalLayout /></MemoryRouter></ToastProvider>);
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
    render(<ToastProvider><MemoryRouter initialEntries={['/assistant']}><PortalLayout /></MemoryRouter></ToastProvider>);
    expect(screen.queryByRole('link', { name: '项目' })).not.toBeInTheDocument();
  });
});
