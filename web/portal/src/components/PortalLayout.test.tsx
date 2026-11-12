import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { PortalLayout } from './PortalLayout';
import { ToastProvider } from '../app/toast';

const portalLayoutAuthState = vi.hoisted(() => ({ role: 'user' as string }));
vi.mock('../app/auth', () => ({ useAuth: () => ({ user: { name: 'Alice', role: portalLayoutAuthState.role }, signOut: vi.fn() }) }));

describe('PortalLayout', () => {
  afterEach(() => { cleanup(); portalLayoutAuthState.role = 'user'; localStorage.removeItem('morde:pc-context-sidebar-collapsed'); });

  it('provides the shared top controls and links to the same-origin mobile app', () => {
    render(<ToastProvider><MemoryRouter initialEntries={['/hosts']}><PortalLayout /></MemoryRouter></ToastProvider>);

    expect(screen.getByRole('link', { name: '打开手机端' })).toHaveAttribute('href', '/app/');
    expect(screen.getByRole('button', { name: '返回上一页' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '前进' })).toBeDisabled();
    expect(screen.getByRole('button', { name: '折叠侧边栏' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { level: 1, name: '云端电脑' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '工作台' })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: '云端电脑' })).toHaveAttribute('href', '/hosts');
    expect(screen.getByRole('link', { name: '账户' })).toHaveAttribute('href', '/settings');
    expect(screen.getByRole('link', { name: '设置' })).toHaveAttribute('href', '/connections');
    expect(screen.getByRole('link', { name: '个人数据' })).toHaveAttribute('href', '/personal');
    expect(screen.getByRole('link', { name: '目标与计划' })).toHaveAttribute('href', '/goals');
    expect(screen.queryByRole('link', { name: '项目' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '工作区' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: '任务与计划' })).not.toBeInTheDocument();
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
    render(<ToastProvider><MemoryRouter initialEntries={['/hosts']}><PortalLayout /></MemoryRouter></ToastProvider>);
    expect(screen.queryByRole('link', { name: '项目' })).not.toBeInTheDocument();
  });
});
